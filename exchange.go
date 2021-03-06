package ep

import (
    "io"
    "net"
    "time"
    "context"
    "encoding/gob"
    "github.com/satori/go.uuid"
)

var _ = registerGob(&exchange{}, &dataReq{}, &errMsg{})

const (
    sendGather = 1
    sendScatter = 2
    sendBroadcast = 3
    sendPartition = 4
)

// Scatter returns an exchange Runner that scatters its input uniformly to
// all other nodes such that the received datasets are dispatched in a round-
// robin to the nodes.
func Scatter() Runner {
    return &exchange{UID: uuid.NewV4().String(), SendTo: sendScatter}
}

// Gather returns an exchange Runner that gathers all of its input into a
// single node. In all other nodes it will produce no output, but on the main
// node it will be passthrough from all of the other nodes
func Gather() Runner {
    return &exchange{UID: uuid.NewV4().String(), SendTo: sendGather}
}

// Broadcast returns an exchange Runner that duplicates its input to all
// other nodes. The output will be effectively a union of all of the inputs from
// all nodes (order not guaranteed)
func Broadcast() Runner {
    return &exchange{UID: uuid.NewV4().String(), SendTo: sendBroadcast}
}

// exchange is a Runner that exchanges data between peer nodes
type exchange struct {
    UID    string
    SendTo int

    encs []encoder // encoders to all destination connections
    decs []decoder // decoders from all source connections
    conns []io.Closer // all open connections (used for closing)
    encsNext int // Encoders Round Robin next index
    decsNext int // Decoders Round Robin next index
}

func (ex *exchange) Returns() []Type { return []Type{Wildcard} }
func (ex *exchange) Run(ctx context.Context, inp, out chan Dataset) (err error) {
    // thisNode := ctx.Value("ep.ThisNode").(string)
    defer func() { ex.Close(err) }()

    err = ex.Init(ctx)
    if err != nil {
        return
    }

    // receive remote data from peers in a go-routine. Write the final error (or
    // nil) to the channel when done.
    errs := make(chan error)
    go func() {
        defer close(errs)
        for {
            data, err := ex.Receive()
            if err == io.EOF {
                break
            } else if err != nil {
                errs <- err
                return
            }

            out <- data
        }

        errs <- nil
    }()

    // send the local data to the peers, until completion or error. Also listen
    // for the completetion of the received go-routine above. When both sending
    // and receiving is complete, exit. Upon error, exit early.
    rcvDone := false
    sndDone := false
    for err == nil && (!rcvDone || !sndDone) {
        select {
        case data, ok := <- inp:
            if !ok {
                // the input is exhauted. Notify peers that we're done sending
                // data (they will use it to stop listening to data from us).
                ex.EncodeAll(io.EOF)
                sndDone = true

                // inp is closed. If we keep iterating, it will infinitely
                // resolve to (nil, true). Nill-ify it to block it on the next
                // iteration.
                inp = nil
                continue
            }

            err = ex.Send(data)
        case err = <- errs:
            rcvDone = true // errors (or nil) from the receive go-routine
        case <- ctx.Done():
            err = ctx.Err() // context timeout or cancel
        }
    }

    return err
}

// Send a dataset to destination nodes
func (ex *exchange) Send(data Dataset) error {
    switch ex.SendTo {
    case sendScatter:
        return ex.EncodeNext(data)
    case sendPartition:
        return ex.EncodePartition(data)
    default:
        return ex.EncodeAll(data)
    }
}

func (ex *exchange) Receive() (Dataset, error) {
    return ex.DecodeNext()
}

// Close all open connections. If an error object is supplied, it's first
// encoded to all connections before closing.
func (ex *exchange) Close(err error) error {
    var errOut error
    if err != nil {
        errOut = ex.EncodeAll(err)

        // the error below is triggered very infrequently when we hang up too
        // fast. 1ms timeout in case of error is a good tradeoff compared to
        // the complexity of locks or a full hand-shake here.
        time.Sleep(1 * time.Millisecond) // use of closed network connection
    }

    for _, conn := range ex.conns {
        err1 := conn.Close()
        if err1 != nil {
            errOut = err1
        }
    }

    return errOut
}

// Encode an object to all destination connections
func (ex *exchange) EncodeAll(e interface{}) (err error) {
    err, _ = e.(error)
    if err != nil {
        e = &errMsg{err.Error()}
        err = nil
    }

    req := &dataReq{e}
    for _, enc := range ex.encs {
        err1 := enc.Encode(req)
        if err1 != nil {
            err = err1
        }
    }

    return err
}

// Encode an object to the next destination connection in a round robin
func (ex *exchange) EncodeNext(e interface{}) error {
    if len(ex.encs) == 0 {
        return io.ErrClosedPipe
    }

    req := &dataReq{e}
    ex.encsNext = (ex.encsNext + 1) % len(ex.encs)
    return ex.encs[ex.encsNext].Encode(req)
}

// Encode an object to a destination connection selected by partitioning
func (ex *exchange) EncodePartition(e interface{}) error {
    return nil
}

// Decode an object from the next source connection in a round robin
func (ex *exchange) DecodeNext() (Dataset, error) {
    if len(ex.decs) == 0 {
        return nil, io.EOF
    }

    i := (ex.decsNext + 1) % len(ex.decs)

    req := &dataReq{}
    err := ex.decs[i].Decode(req)
    data := req.Payload
    if err == nil {
        err, _ = data.(error)
    }

    if err != nil && err.Error() == io.EOF.Error() {
        err = io.EOF
    }

    if err == io.EOF {
        // remove the current decoder and try again
        ex.decs = append(ex.decs[:i], ex.decs[i + 1:]...)
        return ex.DecodeNext()
    } else if err != nil {
        return nil, err
    }

    ex.decsNext = i
    return data.(Dataset), nil
}

// initialize the connections, encoders & decoders
func (ex *exchange) Init(ctx context.Context) error {
    var err error

    allNodes := ctx.Value("ep.AllNodes").([]string)
    thisNode := ctx.Value("ep.ThisNode").(string)
    masterNode := ctx.Value("ep.MasterNode").(string)
    dist := ctx.Value("ep.Distributer").(interface {
        Connect(addr, uid string) (net.Conn, error)
    })

    targetNodes := allNodes
    if ex.SendTo == sendGather {
        targetNodes = []string{masterNode}
    }

    // open a connection to all target nodes
    var conn net.Conn
    connsMap := map[string]net.Conn{}
    var shortCircuit *shortCircuit
    for _, n := range targetNodes {
        if n == thisNode {
            shortCircuit = newShortCircuit()
            ex.conns = append(ex.conns, shortCircuit)
            ex.encs = append(ex.encs, shortCircuit)
            continue
        }

        msg := "THIS " + thisNode + " OTHER " + n

        conn, err = dist.Connect(n, ex.UID)
        if err != nil {
            return err
        }

        connsMap[n] = conn
        ex.conns = append(ex.conns, conn)
        ex.encs = append(ex.encs, dbgEncoder{gob.NewEncoder(conn), msg})
    }

    // if we're also a destination, listen to all nodes
    for i := 0; shortCircuit != nil && i < len(allNodes); i++ {
        n := allNodes[i]

        if n == thisNode {
            ex.decs = append(ex.decs, shortCircuit)
            continue
        }

        msg := "THIS " + thisNode + " OTHER " + n

        // if we already established a connection to this node from the targets,
        // re-use it. We don't need 2 uni-directional connections.
        if connsMap[n] != nil {
            ex.decs = append(ex.decs, dbgDecoder{gob.NewDecoder(connsMap[n]), msg})
            continue
        }

        conn, err = dist.Connect(n, ex.UID)
        if err != nil {
            return err
        }

        ex.conns = append(ex.conns, conn)
        ex.decs = append(ex.decs, dbgDecoder{gob.NewDecoder(conn), msg})
    }

    return nil
}

// interfqace for gob.Encoder/Decoder. Used to also implement the short-circuit.
type encoder interface { Encode(interface{}) error }
type decoder interface { Decode(interface{}) error }

type dbgEncoder struct { encoder; msg string }
func (enc dbgEncoder) Encode(e interface{}) error {
    // fmt.Println("ENCODE", enc.msg, e)
    err := enc.encoder.Encode(e)
    // fmt.Println("ENCODE DONE", enc.msg, e, err)
    return err
}

type dbgDecoder struct { decoder; msg string }
func (dec dbgDecoder) Decode(e interface{}) error {
    // fmt.Println("DECODE", dec.msg)
    err := dec.decoder.Decode(e)
    // fmt.Println("DECODE DONE", dec.msg, e, err)
    return err
}

// shortCircuit implements io.Closer, encoder and dedocder and provides the
// means to short-circuit internal communications within the same node. This is
// in order to not complicate the generic nature of the exchange code
type shortCircuit struct {
    C chan interface{};
    Closed bool
    all []interface{}
}

func (sc *shortCircuit) Close() error {
    if sc.Closed {
        return nil
    }

    sc.Closed = true
    close(sc.C);
    // fmt.Println("SC: Closed")
    return nil
}

func (sc *shortCircuit) Encode(e interface{}) error {
    if sc.Closed {
        return io.ErrClosedPipe
    }

    sc.C <- e;
    // fmt.Println("SC: Encoded", e)
    return nil
}

func (sc *shortCircuit) Decode(e interface{}) error {
    req := e.(*dataReq)
    v, ok := <- sc.C
    if !ok {
        return io.EOF
    }

    req.Payload = v.(*dataReq).Payload
    return nil
}

func newShortCircuit() *shortCircuit {
    return &shortCircuit{C: make(chan interface{}, 1000)}
}

type dataReq struct { Payload interface{} }
type errMsg struct { Msg string }
func (err *errMsg) Error() string { return err.Msg }

package ep

import (
    "io"
    "net"
    "fmt"
    "sync"
    "time"
    "context"
    "encoding/gob"
)

var _ = registerGob(&distRunner{})

// Distributer is an object that can distribute Runners to run in parallel on
// multiple nodes.
type Distributer interface {

    // Distribute a Runner to multiple node addresses. `this` is the address of
    // the current node issuing this distribution.
    Distribute(runner Runner, addrs ...string) Runner

    // Start listening for incoming Runners to run
    Start() error // blocks.

    // Stop listening for incoming Runners to run, and close all open
    // connections.
    Close() error
}

type dialer interface {
    Dial(network, addr string) (net.Conn, error)
}

// NewDistributer creates a Distributer that can be used to distribute work of
// Runners across multiple nodes in a cluster. Distributer must be started on
// all node peers in order for them to receive work. You can also implement the
// dialer interface (implemented by net.Dialer) in order to provide your own
// connections:
//
//      type dialer interface {
//          Dial(network, addr string) (net.Conn, error)
//      }
//
func NewDistributer(addr string, listener net.Listener) Distributer {
    return &distributer{listener, addr, make(map[string]chan net.Conn), &sync.Mutex{}, nil}
}

type distributer struct {
    listener net.Listener
    addr string
    connsMap map[string]chan net.Conn
    l sync.Locker
    closeCh chan error
}

func (d *distributer) Start() error {
    d.l.Lock()
    d.closeCh = make(chan error, 1)
    defer close(d.closeCh)
    d.l.Unlock()

    for {
        conn, err := d.listener.Accept()
        if err != nil {
            return err
        }

        go d.Serve(conn)
    }
}

func (d *distributer) Close() error {
    err := d.listener.Close()
    if err != nil {
        return err
    }

    // wait for Start() above to exit. otherwise, tests or code that attempts to
    // re-bind to the same address will infrequently fail with "bind: address
    // already in use" because while the listener is closed, there's still one
    // pending Accept()
    // TODO: consider waiting for all served connections/runners?
    d.l.Lock()
    defer d.l.Unlock()
    if d.closeCh != nil {
        <- d.closeCh
    }
    return nil
}

func (d *distributer) dial(addr string) (net.Conn, error) {
    dialer, ok := d.listener.(dialer)
    if ok {
        return dialer.Dial("tcp", addr)
    }

    return net.Dial("tcp", addr)
}

func (d *distributer) Distribute(runner Runner, addrs ...string) Runner {
    return &distRunner{runner, addrs, d.addr, d}
}

// Connect to a node address for the given uid. Used by the individual exchange
// runners to synchronize a specific logical point in the code. We need to
// ensure that both sides of the connection, when used with the same UID,
// resolve to the same connection
func (d *distributer) Connect(addr string, uid string) (conn net.Conn, err error) {
    from := d.addr
    if from < addr {
        // dial
        conn, err = d.dial(addr)
        if err != nil {
            return
        }

        err = writeStr(conn, "D") // Data connection
        if err != nil {
            return
        }

        err = writeStr(conn, d.addr + ":" + uid)
        if err != nil {
            return
        }
    } else {
        // listen, timeout after 1 second
        timer := time.NewTimer(time.Second)
        defer timer.Stop()

        select {
        case conn = <- d.connCh(addr + ":" + uid):
            // let it through
        case <- timer.C:
            err = fmt.Errorf("ep: connect timeout; no incoming conn")
        }
    }

    return conn, err
}

func (d *distributer) Serve(conn net.Conn) error {
    typee, err := readStr(conn)
    if err != nil {
        return err
    }

    if typee == "D" { // data connection
        key, err := readStr(conn)
        if err != nil {
            return err
        }

        // wait for someone to claim it.
        d.connCh(key) <- conn
    } else if (typee == "X") { // execute runner connection
        defer conn.Close()

        r := &distRunner{d: d}
        dec := gob.NewDecoder(conn)
        err := dec.Decode(r)
        if err != nil {
            fmt.Println("ep: distributer error", err)
            return err
        }

        out := make(chan Dataset)
        inp := make(chan Dataset, 1)
        close(inp)

        err = r.Run(context.Background(), inp, out)
        if err != nil {
            fmt.Println("ep: runner error", err)
            return err
        }
    } else {
        defer conn.Close()
        
        err := fmt.Errorf("unrecognized connection type: %s", typee)
        fmt.Println("ep: " + err.Error())
        return err
    }

    return nil
}

func (d *distributer) connCh(k string) (chan net.Conn) {
    // k := addr + ":" + uid
    d.l.Lock()
    defer d.l.Unlock()
    if d.connsMap[k] == nil {
        d.connsMap[k] = make(chan net.Conn)
    }
    return d.connsMap[k]
}

// distRunner wraps around a runner, and upon the initial call to Run, it
// distributes the runner to all nodes and runs them in parallel.
type distRunner struct {
    Runner
    Addrs []string // participating node addresses
    MasterAddr string // the master node that created the distRunner
    d *distributer
}

func (r *distRunner) Run(ctx context.Context, inp, out chan Dataset) error {
    isMain := r.d.addr == r.MasterAddr
    for i := 0 ; i < len(r.Addrs) && isMain ; i++ {
        addr := r.Addrs[i]
        if addr == r.d.addr {
            continue
        }

        conn, err := r.d.dial(addr)
        if err != nil {
            return err
        }

        err = writeStr(conn, "X") // runner connection
        if err != nil {
            return err
        }

        defer conn.Close()
        if err != nil {
            return err
        }

        enc := gob.NewEncoder(conn)
        err = enc.Encode(r)
        if err != nil {
            return err
        }
    }

    ctx = context.WithValue(ctx, "ep.AllNodes", r.Addrs)
    ctx = context.WithValue(ctx, "ep.MasterNode", r.MasterAddr)
    ctx = context.WithValue(ctx, "ep.ThisNode", r.d.addr)
    ctx = context.WithValue(ctx, "ep.Distributer", r.d)

    return r.Runner.Run(ctx, inp, out)
}


// write a null-terminated string to a writer
func writeStr(w io.Writer, s string) error {
    _, err := w.Write(append([]byte(s), 0))
    return err
}

// read a null-terminated string from a reader
func readStr(r io.Reader) (s string, err error) {
    b := []byte{0}
    for {
        _, err = r.Read(b)
        if err != nil {
            return
        } else if b[0] == 0 {
            return
        }

        s += string(b[0])
    }
}

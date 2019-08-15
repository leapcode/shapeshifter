package shapeshifter

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/OperatorFoundation/shapeshifter-transports/transports/obfs4"
)

type ShapeShifter struct {
	Cert      string
	IatMode   int
	Target    string // remote ip:port obfs4 server
	SocksAddr string // -proxylistenaddr in shapeshifter-dispatcher
	ln        net.Listener
	errChan   chan error
}

func (ss *ShapeShifter) Open() error {
	err := ss.checkOptions()
	if err != nil {
		return err
	}

	ss.ln, err = net.Listen("tcp", ss.SocksAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %s", err.Error())
	}

	go ss.clientAcceptLoop()
	return nil
}

func (ss *ShapeShifter) Close() error {
	if ss.ln != nil {
		return ss.ln.Close()
	}
	if ss.errChan != nil {
		close(ss.errChan)
	}
	return nil
}

func (ss *ShapeShifter) GetErrorChannel() chan error {
	if ss.errChan == nil {
		ss.errChan = make(chan error, 2)
	}
	return ss.errChan
}

func (ss *ShapeShifter) GetLastError() error {
	if ss.errChan == nil {
		ss.errChan = make(chan error, 2)
	}
	return <-ss.errChan
}

func (ss ShapeShifter) clientAcceptLoop() error {
	for {
		conn, err := ss.ln.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				return err
			}
			ss.sendError("Error accepting connection: %v", err)
			continue
		}
		go ss.clientHandler(conn)
	}
}

func (ss ShapeShifter) clientHandler(conn net.Conn) {
	defer conn.Close()

	transport := obfs4.NewObfs4Client(ss.Cert, ss.IatMode)
	remote, err := transport.Dial(ss.Target)
	if err != nil {
		ss.sendError("outgoing connection failed %s: %v", ss.Target, err)
		return
	}
	if remote == nil {
		ss.sendError("outgoing connection failed %s", ss.Target)
		return
	}
	defer remote.Close()

	err = copyLoop(conn, remote)
	if err != nil {
		ss.sendError("%s - closed connection: %v", ss.Target, err)
	} else {
		log.Printf("%s - closed connection", ss.Target)
	}

	return
}

func copyLoop(a net.Conn, b net.Conn) error {
	// Note: b is always the pt connection.  a is the SOCKS/ORPort connection.
	errChan := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer b.Close()
		defer a.Close()
		_, err := io.Copy(b, a)
		errChan <- err
	}()
	go func() {
		defer wg.Done()
		defer a.Close()
		defer b.Close()
		_, err := io.Copy(a, b)
		errChan <- err
	}()

	// Wait for both upstream and downstream to close.  Since one side
	// terminating closes the other, the second error in the channel will be
	// something like EINVAL (though io.Copy() will swallow EOF), so only the
	// first error is returned.
	wg.Wait()
	if len(errChan) > 0 {
		return <-errChan
	}

	return nil
}

func (ss *ShapeShifter) checkOptions() error {
	if ss.SocksAddr == "" {
		ss.SocksAddr = "127.0.0.1:0"
	}
	if ss.Cert == "" {
		return fmt.Errorf("obfs4 transport missing cert argument")
	}
	return nil
}

func (ss *ShapeShifter) sendError(format string, a ...interface{}) {
	if ss.errChan == nil {
		ss.errChan = make(chan error, 2)
	}
	select {
	case ss.errChan <- fmt.Errorf(format, a...):
	default:
		log.Printf(format, a...)
	}
}

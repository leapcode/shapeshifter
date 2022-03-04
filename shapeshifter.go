package shapeshifter // import "0xacab.org/leap/shapeshifter"

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"

	pt "git.torproject.org/pluggable-transports/goptlib.git"
	"gitlab.com/yawning/obfs4.git/common/ntor"
	"gitlab.com/yawning/obfs4.git/transports/obfs4"
	"golang.org/x/net/proxy"
)

const (
	certLength = ntor.NodeIDLength + ntor.PublicKeyLength
)

func unpackCert(cert string) (*ntor.NodeID, *ntor.PublicKey, error) {
	if l := base64.RawStdEncoding.DecodedLen(len(cert)); l != certLength {
		return nil, nil, fmt.Errorf("cert length %d is invalid", l)
	}
	decoded, err := base64.RawStdEncoding.DecodeString(cert)
	if err != nil {
		return nil, nil, err
	}

	nodeID, _ := ntor.NewNodeID(decoded[:ntor.NodeIDLength])
	pubKey, _ := ntor.NewPublicKey(decoded[ntor.NodeIDLength:])
	return nodeID, pubKey, nil
}

type Logger interface {
	Log(msg string)
}

type ShapeShifter struct {
	Cert      string
	IatMode   int
	Target    string // remote ip:port obfs4 server
	SocksAddr string // -proxylistenaddr in shapeshifter-dispatcher
	Logger    Logger
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
	var err error
	if ss.ln != nil {
		err = ss.ln.Close()
	}
	if ss.errChan != nil {
		close(ss.errChan)
	}
	return err
}

func (ss *ShapeShifter) GetErrorChannel() chan error {
	if ss.errChan == nil {
		ss.errChan = make(chan error, 2)
	}
	return ss.errChan
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

	dialer := proxy.Direct
	// The empty string is the StateDir argument which appears unused on the
	// client side. I am unsure why the clientfactory requires it; possibly to
	// satisfy an interface somewhere, but this is not documented.
	//transport, err := obfs4.NewObfs4Client(ss.Cert, ss.IatMode, dialer)

	transport, err := (&obfs4.Transport{}).ClientFactory("")
	if err != nil {
		ss.sendError("Can not create an obfs4 client (cert: %s, iat-mode: %d): %v", ss.Cert, ss.IatMode, err)
		return
	}
	ptArgs := make(pt.Args)
	nodeID, pubKey, err := unpackCert(ss.Cert)
	if err != nil {
		ss.sendError("Error unpacking cert: %v", err)
		return
	}
	ptArgs.Add("node-id", nodeID.Hex())
	ptArgs.Add("public-key", pubKey.Hex())
	ptArgs.Add("iat-mode", strconv.Itoa(ss.IatMode))
	args, err := transport.ParseArgs(&ptArgs)
	if err != nil {
		ss.sendError("Cannot parse arguments: %v", err)
		return
	}
	remote, err := transport.Dial("tcp", ss.Target, dialer.Dial, args)
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
	_, _, err := unpackCert(ss.Cert)
	return err
}

func (ss *ShapeShifter) sendError(format string, a ...interface{}) {
	if ss.Logger != nil {
		ss.Logger.Log(fmt.Sprintf(format, a...))
		return
	}

	if ss.errChan == nil {
		ss.errChan = make(chan error, 2)
	}
	select {
	case ss.errChan <- fmt.Errorf(format, a...):
	default:
		log.Printf(format, a...)
	}
}

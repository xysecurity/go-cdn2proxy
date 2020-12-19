package cdn2proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/websocket"
)

var (
	DestAddr = "127.0.0.1:8000"
)

// use this logger
var Logger = log.New(os.Stderr, "cdn2proxy server: ", log.Ldate)

// StartServer start websocket server
// port: listen on 127.0.0.1:port
// destAddr: send everything here, we only want a single purpose proxy
func StartServer(port, destAddr string, logOutput io.Writer) (err error) {
	// set DestAddr
	DestAddr = destAddr

	// set log output
	Logger = log.New(logOutput, "cdn2proxy server: ", log.Ldate)

	// HTTP server
	Logger.Printf("websocket server listening on 127.0.0.1:%s", port)
	http.Handle("/ws", websocket.Handler(serveWS))
	err = http.ListenAndServe("127.0.0.1:"+port, nil)
	if err != nil {
		Logger.Fatal(err)
	}
	return
}

func serveWS(ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	Logger.Printf("Got a connection to websocket server: %s", ws.RemoteAddr())
	defer func() {
		cancel()
		ws.Close()
	}()

	// connect to destination
	conn, err := net.Dial("tcp", DestAddr)
	if err != nil {
		Logger.Printf("Cannot dial destination %s: %v", DestAddr, err)
		return
	}
	defer conn.Close()

	go func() {
		defer cancel()
		_, err := io.Copy(conn, ws)
		if err != nil {
			Logger.Printf("serveWS ioCopy ws->dest: %v", err)
			return
		}
	}()
	go func() {
		defer cancel()
		_, err := io.Copy(ws, conn)
		if err != nil {
			Logger.Printf("serveWS ioCopy dest->ws: %v", err)
			return
		}
	}()

	// keep the connection
	for ctx.Err() == nil {
		time.Sleep(1e9)
	}
}

// StartProxy on client side, start a socks5 proxy
// url: websocket server
// addr: local proxy address
func StartProxy(addr, url string) error {
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		Logger.Printf("Cannot listen on %s: %v", addr, err)
		cancel()
		return err
	}
	Logger.Printf("socks proxy listening on %s", addr)
	defer func() {
		cancel()
		listener.Close()
	}()

	for ctx.Err() == nil {
		ws, err := websocket.Dial(url, "", "http://localhost/")
		if err != nil {
			Logger.Printf("websocket connection to %s failed: %v", url, err)
			cancel()
			return err
		}

		conn, err := listener.Accept()
		if err != nil {
			Logger.Print(err)
			cancel()
			return err
		}
		go handleConn(conn, ws)
	}

	return nil
}

func handleConn(conn net.Conn, ws *websocket.Conn) {
	Logger.Printf("Got a connection to our proxy: %s", conn.RemoteAddr())
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		conn.Close()
		ws.Close()
		cancel()
	}()

	// socks5
	// auth
	if err := Socks5Auth(conn); err != nil {
		fmt.Println("auth error:", err)
		return
	}
	// parse
	buf := make([]byte, 256)

	n, err := io.ReadFull(conn, buf[:4])
	if n != 4 {
		Logger.Print("read header: " + err.Error())
		return
	}

	ver, cmd, _, atyp := buf[0], buf[1], buf[2], buf[3]
	if ver != 5 || cmd != 1 {
		Logger.Print("invalid ver/cmd")
		return
	}

	addr := ""
	switch atyp {
	case 1:
		n, err = io.ReadFull(conn, buf[:4])
		if n != 4 {
			Logger.Print("invalid IPv4: " + err.Error())
			return
		}
		addr = fmt.Sprintf("%d.%d.%d.%d", buf[0], buf[1], buf[2], buf[3])

	case 3:
		n, err = io.ReadFull(conn, buf[:1])
		if n != 1 {
			Logger.Print("invalid hostname: " + err.Error())
			return
		}
		addrLen := int(buf[0])

		n, err = io.ReadFull(conn, buf[:addrLen])
		if n != addrLen {
			Logger.Print("invalid hostname: " + err.Error())
			return
		}
		addr = string(buf[:addrLen])

	case 4:
		Logger.Print("IPv6: no supported yet")
		return

	default:
		Logger.Print("invalid atyp")
		return
	}

	n, err = io.ReadFull(conn, buf[:2])
	if n != 2 {
		Logger.Print("read port: " + err.Error())
		return
	}
	port := binary.BigEndian.Uint16(buf[:2])

	// destination
	destAddrPort := fmt.Sprintf("%s:%d", addr, port)
	Logger.Printf("Client wants to connect to %s", destAddrPort)

	// response
	_, err = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	if err != nil {
		Logger.Print("write rsp: " + err.Error())
		return
	}

	// io copy to websocket
	go func() {
		defer cancel()
		_, err := io.Copy(ws, conn)
		if err != nil {
			Logger.Printf("proxy handleConn ioCopy proxy->websocket: %v", err)
			return
		}
	}()
	go func() {
		defer cancel()
		_, err := io.Copy(conn, ws)
		if err != nil {
			Logger.Printf("proxy handleConn ioCopy websocket->proxy: %v", err)
			return
		}
	}()

	// keep the connection
	for ctx.Err() == nil {
		time.Sleep(1e9)
	}
}

func Socks5Auth(client net.Conn) (err error) {
	buf := make([]byte, 256)

	// read VER and NMETHODS
	n, err := io.ReadFull(client, buf[:2])
	if n != 2 {
		return errors.New("reading header: " + err.Error())
	}

	ver, nMethods := int(buf[0]), int(buf[1])
	if ver != 5 {
		return errors.New("invalid version")
	}

	// read auth methods
	n, err = io.ReadFull(client, buf[:nMethods])
	if n != nMethods {
		return errors.New("reading methods: " + err.Error())
	}

	// no auth
	n, err = client.Write([]byte{0x05, 0x00})
	if n != 2 || err != nil {
		return errors.New("write rsp: " + err.Error())
	}

	return nil
}

package core

import "net"

// Gurve is a bit like the binary layer in http2, it breaks a big block of data into small piece, and send to lower layer.
//
type Gurve struct {
	conn         *net.Conn
	maxFrameSize uint32
}

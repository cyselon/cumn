package core

import (
	"io"
	"log"
	"net"
	"testing"
)

func ReadData(c net.Conn) error {
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		log.Print(buf[:n])
		if err == io.EOF {
			break
		}
	}
	return nil
}

func TestSever(t *testing.T) {

}

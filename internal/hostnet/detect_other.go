//go:build !darwin

package hostnet

import (
	"context"
	"errors"
	"net"
)

func Detect(context.Context) (Network, error) {
	return Network{}, errors.New("network bypass requires macOS")
}
func Direct(context.Context, Network, string, string) (net.Conn, error) {
	return nil, errors.New("network bypass requires macOS")
}

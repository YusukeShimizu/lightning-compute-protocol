package lcpd

import (
	"context"
	"errors"
	"fmt"

	lcpdv1 "github.com/bruwbird/lcp/proto-go/lcpd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn *grpc.ClientConn
	svc  lcpdv1.LCPDServiceClient
}

func Dial(ctx context.Context, addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial lcpd grpc: %w", err)
	}
	conn.Connect()
	if waitErr := waitForReady(ctx, conn); waitErr != nil {
		if closeErr := conn.Close(); closeErr != nil {
			return nil, fmt.Errorf("dial lcpd grpc: %w (close failed: %w)", waitErr, closeErr)
		}
		return nil, fmt.Errorf("dial lcpd grpc: %w", waitErr)
	}
	return &Client{
		conn: conn,
		svc:  lcpdv1.NewLCPDServiceClient(conn),
	}, nil
}

func waitForReady(ctx context.Context, conn *grpc.ClientConn) error {
	for {
		state := conn.GetState()
		switch state {
		case connectivity.Ready:
			return nil
		case connectivity.Shutdown:
			return errors.New("connection is shutdown")
		case connectivity.Idle, connectivity.Connecting, connectivity.TransientFailure:
		}

		if ok := conn.WaitForStateChange(ctx, state); !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			return errors.New("connection state change wait failed")
		}
	}
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) Service() lcpdv1.LCPDServiceClient {
	return c.svc
}

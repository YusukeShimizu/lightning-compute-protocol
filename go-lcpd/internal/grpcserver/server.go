package grpcserver

import (
	"context"
	"net"

	lcpdv1 "github.com/bruwbird/lcp/go-lcpd/gen/go/lcpd/v1"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type Params struct {
	fx.In

	Lifecycle fx.Lifecycle
	Listener  net.Listener
	Service   lcpdv1.LCPDServiceServer
	Logger    *zap.SugaredLogger `optional:"true"`
}

func New(p Params) *grpc.Server {
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "grpcserver")

	server := grpc.NewServer()
	lcpdv1.RegisterLCPDServiceServer(server, p.Service)

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				if err := server.Serve(p.Listener); err != nil {
					logger.Errorw("grpc serve failed", "err", err)
				}
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			server.GracefulStop()
			_ = p.Listener.Close()
			return nil
		},
	})

	return server
}

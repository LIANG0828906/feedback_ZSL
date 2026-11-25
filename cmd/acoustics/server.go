package main

import (
	"context"
	"fmt"

	"roomacoustics/pkg/server"
)

func runServer(ctx context.Context, root, addr string) error {
	fmt.Printf("room acoustics diagnostic page: http://%s/\n", addr)
	return server.ListenAndServe(ctx, addr, root)
}

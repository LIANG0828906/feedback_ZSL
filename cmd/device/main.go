package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"audiocal/device"
)

func main() {
	port := flag.Int("port", 18080, "127.0.0.1 高位监听端口")
	flag.Parse()

	srv, err := device.Listen(*port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "设备服务启动失败:", err)
		os.Exit(1)
	}
	fmt.Printf("模拟设备服务监听 %s (固件格式 %s)\n", srv.Addr(), "ACALFW01 v1")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = srv.Close()
	fmt.Println("\n设备服务已退出")
}

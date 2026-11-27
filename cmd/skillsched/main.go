// skillsched 是战斗技能资源预算与冷却排程的可集成领域组件入口，
// 同时提供 net/http 本地诊断工作台。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"skillsched/internal/engine"
	"skillsched/internal/gen"
	"skillsched/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serveCmd(os.Args[2:])
	case "demo":
		demoCmd()
	case "version":
		fmt.Println("skillsched alpha-1")
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: skillsched [serve|demo|version]")
	fmt.Fprintln(os.Stderr, "  serve   启动 net/http 本地诊断工作台")
	fmt.Fprintln(os.Stderr, "  demo    用固定种子样本打印排程与三路径一致性摘要")
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	dbDir := fs.String("dbdir", ".", "sqlite 检查点目录")
	_ = fs.Parse(args)

	srv, err := web.New(*dbDir)
	if err != nil {
		log.Fatalf("init workbench: %v", err)
	}
	defer srv.Close()
	fmt.Printf("技能排程工作台已启动: http://%s/\n", *addr)
	fmt.Println("页面: /  /ticks  /compare  /invalidate  /checkpoint  /api/truth")
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func demoCmd() {
	sc := gen.Generate("cli-demo", gen.DefaultConfig())
	full, err := engine.RunFull(sc.Rules, sc.Intents, false)
	if err != nil {
		log.Fatal(err)
	}
	tree, err := engine.RunFull(sc.Rules, sc.Intents, true)
	if err != nil {
		log.Fatal(err)
	}
	conc, err := engine.RunConcurrent(sc.Rules, sc.Intents, false, 2, 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("场景=%s 种子=%d 技能=%d 池=%d 组=%d 意图=%d tick=%d\n",
		sc.Name, sc.Seed, len(sc.Rules.Skills), len(sc.Rules.Pools),
		len(sc.Rules.Groups), len(sc.Intents), len(full))
	if t, sk, f, ok := engine.Compare(full, sc.Truth.Ticks); ok {
		fmt.Printf("与生成器真值: 不一致 tick=%d skill=%s field=%s\n", t, sk, f)
	} else {
		fmt.Println("与生成器真值: 逐 tick 一致")
	}
	if _, _, _, ok := engine.Compare(full, tree); ok {
		fmt.Println("表达式树 vs 常量表: 不一致")
	} else {
		fmt.Println("表达式树 vs 常量表: 求值一致")
	}
	if t, sk, f, ok := engine.Compare(full, conc.Ticks); ok {
		fmt.Printf("全量 vs 并发: 不一致 tick=%d skill=%s field=%s\n", t, sk, f)
	} else {
		fmt.Println("全量 vs 并发受限预算: 逐 tick 一致")
	}
}

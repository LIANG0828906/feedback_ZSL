package main

import (
	"flag"
	"fmt"
	"os"

	"diffrender/demo"
	"diffrender/render"
)

func main() {
	defaults := demo.DefaultConfig()
	seed := flag.Int64("seed", defaults.BaseSeed, "base deterministic sample seed")
	tasks := flag.Int("tasks", defaults.Tasks, "number of deterministic tasks")
	out := flag.String("out", defaults.OutDir, "artifact output directory")
	iterations := flag.Int("iterations", defaults.Optim.Iterations, "maximum optimization iterations")
	window := flag.Int("window", defaults.Optim.WindowSize, "checkpoint window size")
	workers := flag.Int("workers", defaults.Optim.Render.MaxWorkers, "maximum render workers")
	flag.Parse()

	cfg := demo.DefaultConfig()
	cfg.BaseSeed = *seed
	cfg.Tasks = *tasks
	cfg.OutDir = *out
	cfg.Optim.Iterations = *iterations
	cfg.Optim.WindowSize = *window
	cfg.Optim.Render = render.Config{
		MaxWorkers: *workers, BlockPixels: 128, QueueCapacity: 8, Retries: 1,
	}
	if cfg.Optim.WindowSize <= 0 || cfg.Optim.Iterations <= 0 || cfg.Optim.Render.MaxWorkers <= 0 {
		fmt.Fprintln(os.Stderr, "window, iterations, and workers must be > 0")
		os.Exit(2)
	}
	if cfg.Optim.Iterations < cfg.Optim.WindowSize*2 {
		fmt.Fprintln(os.Stderr, "iterations must cover at least two windows for pause/resume validation")
		os.Exit(2)
	}
	report, err := demo.Run(cfg)
	if report != nil {
		fmt.Printf("seed=%d tasks=%d paths_equal=%v converged=%v checks=%v\n",
			report.Seed, report.TaskCount, report.AllThreePathsEqual, report.AllConverged, report.AllChecksPassed)
		for _, task := range report.Tasks {
			fmt.Printf("%s initial=%.8g final=%.8g windows=%d exports=%d\n",
				task.Name, task.InitialLoss, task.FinalLoss, len(task.Windows), len(task.Exports))
		}
		fmt.Printf("report=%s\n", *out+"/report.json")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

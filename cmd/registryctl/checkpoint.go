package main

import (
	"os"
	"time"

	"serviceregistry/checkpoint"
	"serviceregistry/registry"
)

func commandCheckpoint(args []string) int {
	if len(args) == 0 {
		return fail(registry.NewError(registry.CodeSchemaField, "checkpoint subcommand is required", registry.ErrorContext{}), exitUsage)
	}
	switch args[0] {
	case "dump":
		flags := newFlagSet("checkpoint dump")
		path := flags.String("path", "", "checkpoint file")
		if err := parseFlags(flags, args[1:]); err != nil {
			return fail(err, exitUsage)
		}
		if *path == "" {
			return fail(registry.NewError(registry.CodeSchemaField, "--path is required", registry.ErrorContext{}), exitUsage)
		}
		info, err := os.Stat(*path)
		if err != nil {
			return fail(err, exitBusiness)
		}
		value, err := (&checkpoint.Store{Path: *path}).Load()
		if err != nil {
			return fail(err, exitBusiness)
		}
		return emit(map[string]any{
			"ok":        true,
			"command":   "checkpoint dump",
			"path":      *path,
			"bytes":     info.Size(),
			"modified":  info.ModTime().Format(time.RFC3339Nano),
			"meta":      value.Meta,
			"instances": len(value.Snapshot.Instances),
			"queue":     len(value.Snapshot.Queue),
			"in_flight": len(value.Snapshot.InFlight),
			"steps":     len(value.Steps),
			"last_tick": value.Snapshot.CommitSeq,
			"next_tick": value.Snapshot.NextTick,
		})
	default:
		return fail(registry.NewError(registry.CodeSchemaField, "unknown checkpoint subcommand", registry.ErrorContext{Path: args[0]}), exitUsage)
	}
}

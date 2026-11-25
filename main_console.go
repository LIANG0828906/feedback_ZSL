//go:build !wails

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"reflect"
)

func main() {
	addr := flag.String("http", "127.0.0.1:18080", "browser fallback address; pass empty to print CLI hints")
	flag.Parse()
	if *addr == "" {
		fmt.Println("LightProbe diagnostics core is ready.")
		fmt.Println("Run `go run ./cmd/diagnostic` for deterministic acceptance and verification reports.")
		fmt.Println("Run `go run . -http=127.0.0.1:18080` for a browser preview.")
		fmt.Println("Build the Wails desktop UI with `wails build -tags wails -s` (static frontend needs no Node build step).")
		return
	}

	app := NewApp()
	mux := http.NewServeMux()
	dist, err := fs.Sub(staticAssets, "frontend/dist")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(dist)))
	mux.HandleFunc("/api/call", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Method string            `json:"method"`
			Args   []json.RawMessage `json:"args"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			writeJSON(writer, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		method := reflect.ValueOf(app).MethodByName(payload.Method)
		if !method.IsValid() {
			writeJSON(writer, map[string]any{"ok": false, "message": "unknown method " + payload.Method})
			return
		}
		methodType := method.Type()
		if len(payload.Args) != methodType.NumIn() {
			writeJSON(writer, map[string]any{"ok": false, "message": "argument count mismatch"})
			return
		}
		args := make([]reflect.Value, 0, methodType.NumIn())
		for i, raw := range payload.Args {
			arg := reflect.New(methodType.In(i))
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, arg.Interface()); err != nil {
					writeJSON(writer, map[string]any{"ok": false, "message": err.Error()})
					return
				}
			}
			args = append(args, arg.Elem())
		}
		results := method.Call(args)
		writer.Header().Set("Content-Type", "application/json")
		if len(results) == 1 {
			_ = json.NewEncoder(writer).Encode(results[0].Interface())
			return
		}
		encoded := make([]any, len(results))
		for i, result := range results {
			encoded[i] = result.Interface()
		}
		_ = json.NewEncoder(writer).Encode(encoded)
	})

	log.Printf("LightProbe browser fallback: http://%s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

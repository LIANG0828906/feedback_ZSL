package tasks

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
)

const DefaultSeed int64 = 28120261119

type Task struct {
	TaskID  string `json:"task_id"`
	BatchID string `json:"batch_id"`
	Amount  int64  `json:"amount"`
}

type Package struct {
	Seed          int64    `json:"seed"`
	BatchCount    int      `json:"batch_count"`
	TasksPerBatch int      `json:"tasks_per_batch"`
	TaskCount     int      `json:"task_count"`
	Batches       []string `json:"batches"`
}

type Batch struct {
	ID    string
	Tasks []Task
}

type ResultRow struct {
	TaskID string `json:"task_id"`
	Amount int64  `json:"amount"`
}

type Result struct {
	BatchID     string
	TaskCount   int
	TotalAmount int64
	Digest      string
	PayloadJSON string
}

func Generate(seed int64, batchCount, tasksPerBatch int) []Batch {
	rng := rand.New(rand.NewSource(seed))
	batches := make([]Batch, batchCount)
	taskNumber := 1
	for b := 0; b < batchCount; b++ {
		batchID := fmt.Sprintf("batch-%03d", b+1)
		batch := Batch{ID: batchID, Tasks: make([]Task, tasksPerBatch)}
		for i := 0; i < tasksPerBatch; i++ {
			batch.Tasks[i] = Task{
				TaskID:  fmt.Sprintf("task-%05d", taskNumber),
				BatchID: batchID,
				Amount:  int64(100 + rng.Intn(99901)),
			}
			taskNumber++
		}
		batches[b] = batch
	}
	return batches
}

func BuildResult(batch Batch) (Result, error) {
	rows := make([]ResultRow, len(batch.Tasks))
	var total int64
	for i, task := range batch.Tasks {
		rows[i] = ResultRow{TaskID: task.TaskID, Amount: task.Amount}
		total += task.Amount
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256([]byte("settlement-v1\n" + batch.ID + "\n" + string(payload)))
	return Result{
		BatchID:     batch.ID,
		TaskCount:   len(rows),
		TotalAmount: total,
		Digest:      hex.EncodeToString(sum[:]),
		PayloadJSON: string(payload),
	}, nil
}

func ParseResult(payload string) ([]ResultRow, error) {
	var rows []ResultRow
	if err := json.Unmarshal([]byte(payload), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func WriteManifest(path string, seed int64, batches []Batch) error {
	pkg := Package{
		Seed:          seed,
		BatchCount:    len(batches),
		TasksPerBatch: len(batches[0].Tasks),
		Batches:       make([]string, len(batches)),
	}
	for i, batch := range batches {
		pkg.Batches[i] = batch.ID
		pkg.TaskCount += len(batch.Tasks)
	}
	data, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func WriteJSONL(path string, batches []Batch) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	enc := json.NewEncoder(writer)
	for _, batch := range batches {
		for _, task := range batch.Tasks {
			if err := enc.Encode(task); err != nil {
				return err
			}
		}
	}
	return writer.Flush()
}

func ReadJSONL(path string) ([]Batch, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	byID := make(map[string]*Batch)
	order := make([]string, 0, 20)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var task Task
		if err := json.Unmarshal([]byte(line), &task); err != nil {
			return nil, fmt.Errorf("decode task: %w", err)
		}
		batch := byID[task.BatchID]
		if batch == nil {
			order = append(order, task.BatchID)
			batch = &Batch{ID: task.BatchID}
			byID[task.BatchID] = batch
		}
		batch.Tasks = append(batch.Tasks, task)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	batches := make([]Batch, 0, len(order))
	for _, id := range order {
		batches = append(batches, *byID[id])
	}
	return batches, nil
}

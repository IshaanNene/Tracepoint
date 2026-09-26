package httprun

import (
	"encoding/csv"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"sync/atomic"

	"github.com/IshaanNene/Tracepoint/internal/config"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Feeder modes.
const (
	feederSequential = "sequential"
	feederRandom     = "random"
	feederUnique     = "unique"
)

// feeder is a CSV file loaded once, before the run. Its first row names the columns.
//
// sequential hands rows out in order and wraps; random draws with the iteration's own
// source, so a run is reproducible from its seed; unique hands each row out once and
// then reports itself exhausted, because a unique value used twice - a sign-up email,
// say - would make the target's second answer a different test.
type feeder struct {
	name    string
	mode    string
	columns map[string]int
	rows    [][]string
	next    atomic.Int64
}

func loadFeeder(f config.Feeder, path string) (*feeder, error) {
	file, err := os.Open(f.File)
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigFeederNotFound, err, "opening feeder %q", f.Name).
			WithPath(path + "/file").
			WithHint("the path is relative to the directory tracepoint runs in")
	}
	defer func() { _ = file.Close() }()

	rd := csv.NewReader(file)
	rd.ReuseRecord = false
	header, err := rd.Read()
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalidValue, err, "feeder %q has no header row", f.Name).
			WithPath(path + "/file").WithHint("the first row of the CSV names its columns")
	}
	fd := &feeder{name: f.Name, mode: f.Mode, columns: make(map[string]int, len(header))}
	for i, col := range header {
		fd.columns[col] = i
	}
	for {
		row, rerr := rd.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, errs.Wrap(errs.CodeConfigInvalidValue, rerr, "reading feeder %q", f.Name).WithPath(path + "/file")
		}
		fd.rows = append(fd.rows, row)
	}
	if len(fd.rows) == 0 {
		return nil, errs.New(errs.CodeConfigInvalidValue, "feeder %q has a header but no rows", f.Name).WithPath(path + "/file")
	}
	if fd.mode == "" {
		fd.mode = feederSequential
	}
	return fd, nil
}

// pick returns one row as column -> value, or false once a unique feeder has run out.
func (f *feeder) pick(rng *rand.Rand) (map[string]string, bool) {
	var row []string
	switch f.mode {
	case feederRandom:
		row = f.rows[rng.IntN(len(f.rows))]
	case feederUnique:
		i := f.next.Add(1) - 1
		if i >= int64(len(f.rows)) {
			return nil, false
		}
		row = f.rows[i]
	default:
		row = f.rows[(f.next.Add(1)-1)%int64(len(f.rows))]
	}
	out := make(map[string]string, len(f.columns))
	for col, i := range f.columns {
		if i < len(row) {
			out[col] = row[i]
		}
	}
	return out, true
}

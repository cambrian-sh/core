package config

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
)

// utf8BOM is the byte-order mark Windows tools put in front of a UTF-8 file
// (Windows PowerShell 5 `Set-Content -Encoding utf8`, Notepad). encoding/json
// rejects it as "invalid character 'ï'".
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// StripBOM returns b without a leading UTF-8 byte-order mark. Every config file
// the kernel reads passes through it: the BOM carries no meaning in a UTF-8 file
// and the tools operators edit configs with write it (ENG-2121).
func StripBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, utf8BOM)
}

// ErrMalformedConfigFile marks an error for a config file that exists but does
// not parse. A present file the operator wrote is never skipped: boot stops and
// names it (owner decision 2026-11-18, ENG-2121). Before, loadIfPresent dropped
// the parse error and a providers.json saved with a BOM booted a kernel with no
// LLM behind a green /healthz.
var ErrMalformedConfigFile = errors.New("config file does not parse")

// MalformedJSONError wraps a JSON decode error for the file at path into an
// error that names the file and, when the decoder reports an offset, the line
// and column of the fault in content (the bytes that were decoded, BOM already
// stripped — the column is then the operator's visible column). It wraps both
// ErrMalformedConfigFile and err.
func MalformedJSONError(path string, content []byte, err error) error {
	var offset int64 = -1
	var se *stdjson.SyntaxError
	var te *stdjson.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		offset = se.Offset
	case errors.As(err, &te):
		offset = te.Offset
	}
	if offset >= 0 {
		line, col := lineCol(content, offset)
		return fmt.Errorf("%w: %s (line %d, column %d): %w", ErrMalformedConfigFile, path, line, col, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrMalformedConfigFile, path, err)
}

// lineCol converts a byte offset reported by encoding/json (the count of bytes
// read when the fault was seen) into a 1-based line and column.
func lineCol(b []byte, offset int64) (int, int) {
	if offset > int64(len(b)) {
		offset = int64(len(b))
	}
	line, col := 1, 1
	for _, c := range b[:offset] {
		if c == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	// The offset points just past the offending byte; report that byte.
	if col > 1 {
		col--
	}
	return line, col
}

// checkConfigObject reports whether b decodes as one config layer — a JSON
// object, which is the only shape koanf merges — and returns a
// MalformedJSONError naming path when it does not.
func checkConfigObject(path string, b []byte) error {
	var doc map[string]any
	if err := stdjson.Unmarshal(b, &doc); err != nil {
		return MalformedJSONError(path, b, err)
	}
	return nil
}

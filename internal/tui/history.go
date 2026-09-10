package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const historyFileName = "history"

func loadHistory(path string) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var history []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var message string
		if err := json.Unmarshal(line, &message); err != nil {
			// Keep plain text lines readable and compatible with a hand-created
			// history file from an earlier version.
			message = string(line)
		}
		if strings.TrimSpace(message) != "" {
			history = append(history, message)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return history, nil
}

func appendHistory(path, message string) error {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(encoded)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.ErrShortWrite) {
		return io.ErrShortWrite
	}
	return err
}

// Package csvexporter provides a k6 extension for CSV export.
package csvexporter

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"

	"go.k6.io/k6/js/modules"
)

// Register the extension with k6
func init() {
	modules.Register("k6/x/csv-exporter", new(RootModule))
}

// RootModule is the global module instance
type RootModule struct{}

// NewModuleInstance returns a new instance of the module for each VU
func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	return &ModuleInstance{
		vu: vu,
	}
}

// ModuleInstance represents an instance of the module for each VU
type ModuleInstance struct {
	vu modules.VU
}

// Exports returns the exports of the module
func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{
		Default: &CSVExporter{},
	}
}

// CSVExporter is the object exposed to JS
type CSVExporter struct{}

// WriteToFile writes data to a CSV file
// JS signature: csv.writeToFile(filename, data[], delimiter)
func (c *CSVExporter) WriteToFile(filename string, data interface{}, delimiter string) (int, error) {
	rows, ok := data.([]interface{})
	if !ok || len(rows) == 0 {
		return 0, fmt.Errorf("data must be a non-empty array of objects")
	}

	firstRow, ok := rows[0].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("array elements must be objects")
	}

	// Get headers (sorted for deterministic order)
	headers := make([]string, 0, len(firstRow))
	for k := range firstRow {
		headers = append(headers, k)
	}
	sort.Strings(headers)

	// Create file
	f, err := os.Create(filename)
	if err != nil {
		return 0, fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	// Write UTF-8 BOM for Excel compatibility
	if _, err := f.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return 0, fmt.Errorf("write BOM: %w", err)
	}

	w := csv.NewWriter(f)
	if delimiter != "" && len(delimiter) > 0 {
		w.Comma = []rune(delimiter)[0]
	} else {
		w.Comma = ';'
	}

	// Write headers
	if err := w.Write(headers); err != nil {
		return 0, fmt.Errorf("write headers: %w", err)
	}

	// Write data rows
	written := 0
	for _, row := range rows {
		obj, ok := row.(map[string]interface{})
		if !ok {
			continue
		}
		record := make([]string, len(headers))
		for i, h := range headers {
			if val, ok := obj[h]; ok && val != nil {
				record[i] = fmt.Sprintf("%v", val)
			}
		}
		if err := w.Write(record); err != nil {
			return written, fmt.Errorf("write row: %w", err)
		}
		written++
	}

	w.Flush()
	return written, w.Error()
}
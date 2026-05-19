package csvexporter

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	k6modules "go.k6.io/k6/js/modules"
)

// Убедимся, что тип реализует интерфейс k6modules.Module
var _ k6modules.Module = new(Module)

// Module - точка входа для xk6
type Module struct{}

func New() *Module {
	return &Module{}
}

func (m *Module) NewModuleInstance(vu k6modules.VU) k6modules.ModuleInstance {
	return &ModuleInstance{vu: vu}
}

type ModuleInstance struct {
	vu k6modules.VU
}

// Exports определяет, что будет доступно в JS
func (mi *ModuleInstance) Exports() k6modules.Exports {
	return k6modules.Exports{
		Default: &CSVExporter{},
	}
}

// CSVExporter - объект, методы которого вызываются из JS
type CSVExporter struct {
	mu sync.Mutex // Защита от гонок при параллельной записи в один файл
}

// WriteToFile записывает массив объектов в CSV
// Сигнатура для JS: csv.writeToFile("output.csv", dataArray, ";")
func (c *CSVExporter) WriteToFile(filename string, data interface{}, delimiter string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if data == nil {
		return 0, fmt.Errorf("data cannot be nil")
	}

	// Преобразование JS-массива в Go-срез
	rows, ok := data.([]interface{})
	if !ok {
		return 0, fmt.Errorf("data must be an array of objects")
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("data array is empty")
	}

	// Извлечение заголовков из первой строки
	firstRow, ok := rows[0].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("array elements must be objects")
	}

	headers := make([]string, 0, len(firstRow))
	for k := range firstRow {
		headers = append(headers, k)
	}
	// Сортировка для детерминированного порядка колонок
	sort.Strings(headers)

	// Создание файла
	f, err := os.Create(filename)
	if err != nil {
		return 0, fmt.Errorf("failed to create file %s: %w", filename, err)
	}
	defer f.Close()

	// Запись UTF-8 BOM для корректного отображения кириллицы в Excel
	if _, err := f.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return 0, fmt.Errorf("failed to write BOM: %w", err)
	}

	// Инициализация CSV-записи
	w := csv.NewWriter(f)
	if delimiter != "" {
		w.Comma = []rune(delimiter)[0]
	} else {
		w.Comma = ';'
	}

	// Запись заголовков
	if err := w.Write(headers); err != nil {
		return 0, fmt.Errorf("failed to write headers: %w", err)
	}

	// Запись строк
	writtenRows := 0
	for _, row := range rows {
		obj, ok := row.(map[string]interface{})
		if !ok {
			continue
		}

		record := make([]string, len(headers))
		for i, h := range headers {
			val := obj[h]
			if val == nil {
				record[i] = ""
			} else {
				record[i] = fmt.Sprintf("%v", val)
			}
		}

		if err := w.Write(record); err != nil {
			return writtenRows, fmt.Errorf("failed to write row: %w", err)
		}
		writtenRows++
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return writtenRows, fmt.Errorf("csv flush error: %w", err)
	}

	return writtenRows, nil
}
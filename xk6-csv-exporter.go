package csvexporter

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"go.k6.io/k6/js/modules"

	_ "github.com/sijms/go-ora/v2"
)

func init() {
	modules.Register("k6/x/csv-exporter", new(RootModule))
}

type RootModule struct{}

func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	return &ModuleInstance{vu: vu}
}

type ModuleInstance struct {
	vu modules.VU
}

func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{
		Default: &CSVExporter{},
	}
}

type CSVExporter struct{}

// 🔹 Глобальная переменная для блокировки (обязательна для многопоточной записи)
var globalFileMutex sync.Mutex

// ============================================================================
// 🔹 МЕТОДЫ ДЛЯ МАССИВОВ ДАННЫХ
// ============================================================================

func (c *CSVExporter) WriteToFile(filename string, data interface{}, delimiter string) (int, error) {
	return c.writeToFileInternal(filename, data, delimiter, false, false)
}

func (c *CSVExporter) WriteToFileWithBom(filename string, data interface{}, delimiter string) (int, error) {
	return c.writeToFileInternal(filename, data, delimiter, true, false)
}

func (c *CSVExporter) AppendToFile(filename string, data interface{}, delimiter string) (int, error) {
	return c.writeToFileInternal(filename, data, delimiter, false, true)
}

func (c *CSVExporter) AppendToFileWithBom(filename string, data interface{}, delimiter string) (int, error) {
	return c.writeToFileInternal(filename, data, delimiter, true, true)
}

func (c *CSVExporter) writeToFileInternal(filename string, data interface{}, delimiter string, withBom bool, isAppend bool) (int, error) {
	rows, ok := data.([]interface{})
	if !ok || len(rows) == 0 {
		return 0, fmt.Errorf("data must be a non-empty array of objects")
	}

	firstRow, ok := rows[0].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("array elements must be objects")
	}

	// Сортировка заголовков (здесь используется import "sort")
	headers := make([]string, 0, len(firstRow))
	for k := range firstRow {
		headers = append(headers, k)
	}
	sort.Strings(headers)

	// Блокировка для безопасной многопоточной записи
	globalFileMutex.Lock()
	defer globalFileMutex.Unlock()

	var f *os.File
	var err error

	if isAppend {
		f, err = os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		f, err = os.Create(filename)
	}
	
	if err != nil {
		return 0, fmt.Errorf("open/create file: %w", err)
	}
	defer f.Close()

	fileInfo, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat file: %w", err)
	}

	isNewFile := fileInfo.Size() == 0

	if isNewFile && withBom {
		if _, err := f.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			return 0, fmt.Errorf("write BOM: %w", err)
		}
	}

	w := csv.NewWriter(f)
	if delimiter != "" && len(delimiter) > 0 {
		w.Comma = []rune(delimiter)[0]
	} else {
		w.Comma = ';'
	}

	if isNewFile {
		if err := w.Write(headers); err != nil {
			return 0, fmt.Errorf("write headers: %w", err)
		}
	}

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

// ============================================================================
// 🔹 МЕТОДЫ ДЛЯ PL/SQL
// ============================================================================

func (c *CSVExporter) ExecPlSqlToCsv(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, false, false)
}

func (c *CSVExporter) ExecPlSqlToCsvWithBom(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, true, false)
}

func (c *CSVExporter) AppendPlSqlToCsv(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, false, true)
}

func (c *CSVExporter) AppendPlSqlToCsvWithBom(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, true, true)
}

func (c *CSVExporter) execPlSqlToCsvInternal(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}, withBom bool, isAppend bool) (int, error) {
	db, err := sql.Open("oracle", connStr)
	if err != nil {
		return 0, fmt.Errorf("connection failed: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return 0, fmt.Errorf("ping failed: %w", err)
	}

	createSeqSQL := `DECLARE v_count NUMBER; BEGIN SELECT COUNT(*) INTO v_count FROM user_sequences WHERE sequence_name = 'TMP_K6_DBMS_OUTPUT_SEQ'; IF v_count = 0 THEN EXECUTE IMMEDIATE 'CREATE SEQUENCE TMP_K6_DBMS_OUTPUT_SEQ START WITH 1 INCREMENT BY 1'; END IF; END;`
	if _, err := db.Exec(createSeqSQL); err != nil {
		return 0, fmt.Errorf("failed to create sequence: %w", err)
	}

	createTableSQL := `DECLARE v_count NUMBER; BEGIN SELECT COUNT(*) INTO v_count FROM user_tables WHERE table_name = 'TMP_K6_DBMS_OUTPUT'; IF v_count = 0 THEN EXECUTE IMMEDIATE 'CREATE GLOBAL TEMPORARY TABLE TMP_K6_DBMS_OUTPUT (line_data VARCHAR2(4000), line_order NUMBER) ON COMMIT PRESERVE ROWS'; END IF; END;`
	if _, err := db.Exec(createTableSQL); err != nil {
		return 0, fmt.Errorf("failed to create temp table: %w", err)
	}

	if _, err := db.Exec("DELETE FROM TMP_K6_DBMS_OUTPUT"); err != nil {
		return 0, fmt.Errorf("failed to clear temp table: %w", err)
	}

	modifiedCode := plsqlCode
	re := regexp.MustCompile(`(?i)DBMS_OUTPUT\.PUT_LINE\s*\((.*?)\)\s*;`)
	modifiedCode = re.ReplaceAllString(modifiedCode, `INSERT INTO TMP_K6_DBMS_OUTPUT(line_data, line_order) VALUES ($1, TMP_K6_DBMS_OUTPUT_SEQ.NEXTVAL);`)
	modifiedCode = regexp.MustCompile(`(?i)dbms_output\.disable\s*;`).ReplaceAllString(modifiedCode, "-- removed by plugin")
	modifiedCode = regexp.MustCompile(`(?i)dbms_output\.enable\s*\([^)]*\)\s*;`).ReplaceAllString(modifiedCode, "-- removed by plugin")

	if _, err := db.Exec(modifiedCode); err != nil {
		return 0, fmt.Errorf("PL/SQL execution failed: %w", err)
	}

	rows, err := db.Query(`SELECT line_data FROM TMP_K6_DBMS_OUTPUT ORDER BY line_order`)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch output: %w", err)
	}
	defer rows.Close()

	// Блокировка для безопасной многопоточной записи
	globalFileMutex.Lock()
	defer globalFileMutex.Unlock()

	var file *os.File
	if isAppend {
		file, err = os.OpenFile(outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		file, err = os.Create(outputFile)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to open/create CSV file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat file: %w", err)
	}
	isNewFile := fileInfo.Size() == 0

	if isNewFile && withBom {
		if _, err := file.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			return 0, fmt.Errorf("failed to write BOM: %w", err)
		}
	}

	writer := csv.NewWriter(file)
	if delimiter != "" && len(delimiter) > 0 {
		writer.Comma = []rune(delimiter)[0]
	} else {
		writer.Comma = ';'
	}

	var finalHeaders []string
	var hasCustomHeaders bool

	if headers != nil {
		if hArray, ok := headers.([]interface{}); ok {
			for _, h := range hArray {
				if str, ok := h.(string); ok && str != "" {
					finalHeaders = append(finalHeaders, str)
				}
			}
			if len(finalHeaders) > 0 {
				hasCustomHeaders = true
			}
		}
	}

	if !hasCustomHeaders {
		finalHeaders = []string{"dbms_output_line"}
	}

	if isNewFile {
		if err := writer.Write(finalHeaders); err != nil {
			return 0, fmt.Errorf("failed to write headers: %w", err)
		}
	}

	count := 0
	for rows.Next() {
		var lineData string
		if err := rows.Scan(&lineData); err != nil {
			continue
		}

		var record []string
		if hasCustomHeaders {
			parts := strings.Split(lineData, string(writer.Comma))
			record = make([]string, len(finalHeaders))
			for i := 0; i < len(finalHeaders) && i < len(parts); i++ {
				record[i] = strings.TrimSpace(parts[i])
			}
		} else {
			record = []string{strings.TrimSpace(lineData)}
		}

		if err := writer.Write(record); err != nil {
			return count, fmt.Errorf("failed to write row: %w", err)
		}
		count++
	}

	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("error during row iteration: %w", err)
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return count, fmt.Errorf("CSV flush error: %w", err)
	}

	return count, nil
}
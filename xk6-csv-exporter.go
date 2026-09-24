package csvexporter

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"go.k6.io/k6/js/modules"

	_ "github.com/sijms/go-ora/v2" // Oracle driver
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

// ============================================================================
// 🔹 МЕТОДЫ ДЛЯ PL/SQL (ExecPlSqlToCsv / AppendPlSqlToCsv)
// ============================================================================

// ExecPlSqlToCsv: Однопоточная запись (перезапись)
func (c *CSVExporter) ExecPlSqlToCsv(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, false, false)
}

// ExecPlSqlToCsvWithBom: Однопоточная запись с BOM
func (c *CSVExporter) ExecPlSqlToCsvWithBom(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, true, false)
}

// AppendPlSqlToCsv: МНОГОПОТОЧНАЯ запись (добавление)
func (c *CSVExporter) AppendPlSqlToCsv(connStr string, plsqlCode string, outputFile string, delimiter string, headers interface{}) (int, error) {
	return c.execPlSqlToCsvInternal(connStr, plsqlCode, outputFile, delimiter, headers, false, true)
}

// AppendPlSqlToCsvWithBom: Многопоточная запись с BOM
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

	// 1. Создание sequence и GTT
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

	// 2. Модификация PL/SQL-кода
	modifiedCode := plsqlCode
	re := regexp.MustCompile(`(?i)DBMS_OUTPUT\.PUT_LINE\s*\((.*?)\)\s*;`)
	modifiedCode = re.ReplaceAllString(modifiedCode, `INSERT INTO TMP_K6_DBMS_OUTPUT(line_data, line_order) VALUES ($1, TMP_K6_DBMS_OUTPUT_SEQ.NEXTVAL);`)
	modifiedCode = regexp.MustCompile(`(?i)dbms_output\.disable\s*;`).ReplaceAllString(modifiedCode, "-- removed by plugin")
	modifiedCode = regexp.MustCompile(`(?i)dbms_output\.enable\s*\([^)]*\)\s*;`).ReplaceAllString(modifiedCode, "-- removed by plugin")

	if _, err := db.Exec(modifiedCode); err != nil {
		return 0, fmt.Errorf("PL/SQL execution failed: %w", err)
	}

	// 3. Чтение данных
	rows, err := db.Query(`SELECT line_data FROM TMP_K6_DBMS_OUTPUT ORDER BY line_order`)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch output: %w", err)
	}
	defer rows.Close()

	// 4. БЛОКИРОВКА: Гарантируем безопасную многопоточную запись в файл
	globalFileMutex.Lock()
	defer globalFileMutex.Unlock()

	var file *os.File
	if isAppend {
		// Открываем в режиме добавления (создаем, если не существует)
		file, err = os.OpenFile(outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		// Режим перезаписи (для setup)
		file, err = os.Create(outputFile)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to open/create CSV file: %w", err)
	}
	defer file.Close()

	// Проверяем, пустой ли файл, чтобы решить, писать ли заголовки и BOM
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

	// 5. Обработка хедеров
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

	// Записываем заголовки ТОЛЬКО если файл был пустым
	if isNewFile {
		if err := writer.Write(finalHeaders); err != nil {
			return 0, fmt.Errorf("failed to write headers: %w", err)
		}
	}

	// 6. Запись строк
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
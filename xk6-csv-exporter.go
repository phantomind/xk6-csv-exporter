package csvexporter

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"

	"go.k6.io/k6/js/modules"

	_ "github.com/sijms/go-ora/v2"
)

func init() {
	modules.Register("k6/x/csv-exporter", new(RootModule))
}

type RootModule struct{}

func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	return &ModuleInstance{
		vu: vu,
	}
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

func (c *CSVExporter) WriteToFile(filename string, data interface{}, delimiter string) (int, error) {
	rows, ok := data.([]interface{})
	if !ok || len(rows) == 0 {
		return 0, fmt.Errorf("data must be a non-empty array of objects")
	}

	firstRow, ok := rows[0].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("array elements must be objects")
	}

	headers := make([]string, 0, len(firstRow))
	for k := range firstRow {
		headers = append(headers, k)
	}
	sort.Strings(headers)

	f, err := os.Create(filename)
	if err != nil {
		return 0, fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if delimiter != "" && len(delimiter) > 0 {
		w.Comma = []rune(delimiter)[0]
	} else {
		w.Comma = ';'
	}

	if err := w.Write(headers); err != nil {
		return 0, fmt.Errorf("write headers: %w", err)
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

func (c *CSVExporter) ExecPlSqlToCsv(connStr string, plsqlCode string, outputFile string, delimiter string) (int, error) {
	db, err := sql.Open("oracle", connStr)
	if err != nil {
		return 0, fmt.Errorf("connection failed: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return 0, fmt.Errorf("ping failed: %w", err)
	}

	createTableSQL := `
		DECLARE
			v_count NUMBER;
		BEGIN
			SELECT COUNT(*) INTO v_count FROM user_tables WHERE table_name = 'TMP_K6_DBMS_OUTPUT';
			IF v_count = 0 THEN
				EXECUTE IMMEDIATE 'CREATE GLOBAL TEMPORARY TABLE TMP_K6_DBMS_OUTPUT (
					line_data VARCHAR2(4000),
					line_order NUMBER
				) ON COMMIT PRESERVE ROWS';
			END IF;
		END;`
	
	if _, err := db.Exec(createTableSQL); err != nil {
		return 0, fmt.Errorf("failed to create temp table: %w", err)
	}

	if _, err := db.Exec("DELETE FROM TMP_K6_DBMS_OUTPUT"); err != nil {
		return 0, fmt.Errorf("failed to clear temp table: %w", err)
	}

	modifiedCode := strings.ReplaceAll(
		plsqlCode, 
		"DBMS_OUTPUT.PUT_LINE", 
		"INSERT INTO TMP_K6_DBMS_OUTPUT(line_data, line_order) VALUES",
	)

	modifiedCode = strings.Replace(
		modifiedCode,
		"begin",
		"BEGIN\n  NULL; -- DBMS_OUTPUT перехвачен плагином\n",
		1,
	)

	if _, err := db.Exec(modifiedCode); err != nil {
		return 0, fmt.Errorf("PL/SQL execution failed: %w", err)
	}

	rows, err := db.Query(`
		SELECT line_data 
		FROM TMP_K6_DBMS_OUTPUT 
		WHERE line_data LIKE '%;%;%' 
		ORDER BY line_order
	`)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch output: %w", err)
	}
	defer rows.Close()

	file, err := os.Create(outputFile)
	if err != nil {
		return 0, fmt.Errorf("failed to create CSV file: %w", err)
	}
	defer file.Close()


	writer := csv.NewWriter(file)
	if delimiter != "" && len(delimiter) > 0 {
		writer.Comma = []rune(delimiter)[0]
	} else {
		writer.Comma = ';'
	}

	if err := writer.Write([]string{"login", "numberClient", "pass"}); err != nil {
		return 0, fmt.Errorf("failed to write headers: %w", err)
	}

	count := 0
	for rows.Next() {
		var lineData string
		if err := rows.Scan(&lineData); err != nil {
			continue
		}

		parts := strings.Split(lineData, ";")
		if len(parts) >= 3 {
			login := strings.TrimSpace(parts[0])
			pass := strings.TrimSpace(parts[1])
			numberClient := strings.TrimSpace(parts[2])

			record := []string{login, numberClient, pass}
			if err := writer.Write(record); err != nil {
				return count, fmt.Errorf("failed to write row: %w", err)
			}
			count++
		}
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
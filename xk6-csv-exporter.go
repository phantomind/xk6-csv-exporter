package csvexporter

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/metrics"

	_ "github.com/sijms/go-ora/v2"
)

func init() {
	modules.Register("k6/x/csv-exporter", new(RootModule))
}

// ============================================================================
// 🔹 МОДУЛЬ И МЕТРИКИ
// ============================================================================

type RootModule struct{}

func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	mi := &ModuleInstance{vu: vu}
	mi.initMetrics()
	return mi
}

type ModuleInstance struct {
	vu modules.VU

	// SQL метрики (аналог http_req_duration, http_reqs, http_req_failed)
	sqlQueryDuration *metrics.Metric
	sqlQueryCount    *metrics.Metric
	sqlQueryFailed   *metrics.Metric
	sqlRowsReturned  *metrics.Metric

	// CSV метрики
	csvWriteDuration *metrics.Metric
	csvRowsWritten   *metrics.Metric
	csvWriteFailed   *metrics.Metric
}

// initMetrics регистрирует кастомные метрики в k6 Registry
func (mi *ModuleInstance) initMetrics() {
	env := mi.vu.InitEnv()
	if env == nil {
		return // InitEnv недоступен (например, в runtime-контексте)
	}
	registry := env.Registry

	var err error

	// --- SQL метрики ---
	mi.sqlQueryDuration, err = registry.NewMetric("sql_query_duration", metrics.Trend, metrics.Time)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register sql_query_duration")
	}
	mi.sqlQueryCount, err = registry.NewMetric("sql_queries", metrics.Counter, metrics.Default)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register sql_queries")
	}
	mi.sqlQueryFailed, err = registry.NewMetric("sql_query_failed", metrics.Rate, metrics.Default)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register sql_query_failed")
	}
	mi.sqlRowsReturned, err = registry.NewMetric("sql_rows_returned", metrics.Counter, metrics.Default)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register sql_rows_returned")
	}

	// --- CSV метрики ---
	mi.csvWriteDuration, err = registry.NewMetric("csv_write_duration", metrics.Trend, metrics.Time)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register csv_write_duration")
	}
	mi.csvRowsWritten, err = registry.NewMetric("csv_rows_written", metrics.Counter, metrics.Default)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register csv_rows_written")
	}
	mi.csvWriteFailed, err = registry.NewMetric("csv_write_failed", metrics.Rate, metrics.Default)
	if err != nil {
		env.Logger.WithError(err).Warn("failed to register csv_write_failed")
	}
}

func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{
		Default: &CSVExporter{mi: mi},
	}
}

type CSVExporter struct {
	mi *ModuleInstance
}

// 🔹 Глобальная блокировка для многопоточной записи
var globalFileMutex sync.Mutex

// ============================================================================
// 🔹 ХЕЛПЕР ДЛЯ ЗАПИСИ МЕТРИК
// ============================================================================

// pushSample записывает метрику в k6 (аналог встроенного модуля http)
func (c *CSVExporter) pushSample(ctx context.Context, metric *metrics.Metric, value float64, extraTags map[string]string) {
	if metric == nil || c.mi == nil {
		return
	}
	state := c.mi.vu.State()
	if state == nil || state.Samples == nil {
		return
	}

	// 🔹 Безопасное получение тегов
	tags := state.Tags.GetCurrentValues().Tags

	// 🔹 Добавляем кастомные теги через map
	if extraTags != nil {
		for k, v := range extraTags {
			tags = tags.With(k, v)
		}
	}

	// 🔹 Прямая запись sample
	sample := metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: metric,
			Tags:   tags,
		},
		Time:  time.Now(),
		Value: value,
	}
	metrics.PushIfNotDone(ctx, state.Samples, sample)
}

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
	ctx := c.mi.vu.Context()
	startTime := time.Now()
	fileTag := map[string]string{"file": filename, "op": "write"}

	rows, ok := data.([]interface{})
	if !ok || len(rows) == 0 {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return 0, fmt.Errorf("data must be a non-empty array of objects")
	}

	// Если имя файла пустое или "null" — просто возвращаем количество строк
	if filename == "" || filename == "null" {
		return len(rows), nil
	}

	firstRow, ok := rows[0].(map[string]interface{})
	if !ok {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return 0, fmt.Errorf("array elements must be objects")
	}

	headers := make([]string, 0, len(firstRow))
	for k := range firstRow {
		headers = append(headers, k)
	}
	sort.Strings(headers)

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
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return 0, fmt.Errorf("open/create file: %w", err)
	}
	defer f.Close()

	fileInfo, err := f.Stat()
	if err != nil {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return 0, fmt.Errorf("stat file: %w", err)
	}

	isNewFile := fileInfo.Size() == 0

	if isNewFile && withBom {
		if _, err := f.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
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
			c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
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
			c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
			return written, fmt.Errorf("write row: %w", err)
		}
		written++
	}

	w.Flush()
	if err := w.Error(); err != nil {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return written, err
	}

	// 🔹 Успешные метрики
	durationMs := float64(time.Since(startTime).Milliseconds())
	c.pushSample(ctx, c.mi.csvWriteDuration, durationMs, fileTag)
	c.pushSample(ctx, c.mi.csvRowsWritten, float64(written), fileTag)
	c.pushSample(ctx, c.mi.csvWriteFailed, 0, fileTag)

	return written, nil
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
	ctx := c.mi.vu.Context()
	startTime := time.Now()
	sqlTag := map[string]string{"query_type": "plsql"}
	fileTag := map[string]string{"file": outputFile, "op": "plsql"}

	// Вспомогательная функция для фиксации ошибки SQL
	failSQL := func(err error) (int, error) {
		durationMs := float64(time.Since(startTime).Milliseconds())
		c.pushSample(ctx, c.mi.sqlQueryDuration, durationMs, sqlTag)
		c.pushSample(ctx, c.mi.sqlQueryCount, 1, sqlTag)
		c.pushSample(ctx, c.mi.sqlQueryFailed, 1, sqlTag)
		return 0, err
	}

	db, err := sql.Open("oracle", connStr)
	if err != nil {
		return failSQL(fmt.Errorf("connection failed: %w", err))
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return failSQL(fmt.Errorf("ping failed: %w", err))
	}

	createSeqSQL := `DECLARE v_count NUMBER; BEGIN SELECT COUNT(*) INTO v_count FROM user_sequences WHERE sequence_name = 'TMP_K6_DBMS_OUTPUT_SEQ'; IF v_count = 0 THEN EXECUTE IMMEDIATE 'CREATE SEQUENCE TMP_K6_DBMS_OUTPUT_SEQ START WITH 1 INCREMENT BY 1'; END IF; END;`
	if _, err := db.Exec(createSeqSQL); err != nil {
		return failSQL(fmt.Errorf("failed to create sequence: %w", err))
	}

	createTableSQL := `DECLARE v_count NUMBER; BEGIN SELECT COUNT(*) INTO v_count FROM user_tables WHERE table_name = 'TMP_K6_DBMS_OUTPUT'; IF v_count = 0 THEN EXECUTE IMMEDIATE 'CREATE GLOBAL TEMPORARY TABLE TMP_K6_DBMS_OUTPUT (line_data VARCHAR2(4000), line_order NUMBER) ON COMMIT PRESERVE ROWS'; END IF; END;`
	if _, err := db.Exec(createTableSQL); err != nil {
		return failSQL(fmt.Errorf("failed to create temp table: %w", err))
	}

	if _, err := db.Exec("DELETE FROM TMP_K6_DBMS_OUTPUT"); err != nil {
		return failSQL(fmt.Errorf("failed to clear temp table: %w", err))
	}

	modifiedCode := plsqlCode
	re := regexp.MustCompile(`(?i)DBMS_OUTPUT\.PUT_LINE\s*\((.*?)\)\s*;`)
	modifiedCode = re.ReplaceAllString(modifiedCode, `INSERT INTO TMP_K6_DBMS_OUTPUT(line_data, line_order) VALUES ($1, TMP_K6_DBMS_OUTPUT_SEQ.NEXTVAL);`)
	modifiedCode = regexp.MustCompile(`(?i)dbms_output\.disable\s*;`).ReplaceAllString(modifiedCode, "-- removed by plugin")
	modifiedCode = regexp.MustCompile(`(?i)dbms_output\.enable\s*\([^)]*\)\s*;`).ReplaceAllString(modifiedCode, "-- removed by plugin")

	if _, err := db.Exec(modifiedCode); err != nil {
		return failSQL(fmt.Errorf("PL/SQL execution failed: %w", err))
	}

	rows, err := db.Query(`SELECT line_data FROM TMP_K6_DBMS_OUTPUT ORDER BY line_order`)
	if err != nil {
		return failSQL(fmt.Errorf("failed to fetch output: %w", err))
	}
	defer rows.Close()

	// Если файл не нужен — просто считаем строки
	if outputFile == "" || outputFile == "null" {
		count := 0
		for rows.Next() {
			count++
		}
		if err := rows.Err(); err != nil {
			return failSQL(fmt.Errorf("error during row iteration: %w", err))
		}
		// 🔹 Успешные SQL-метрики (без CSV)
		durationMs := float64(time.Since(startTime).Milliseconds())
		c.pushSample(ctx, c.mi.sqlQueryDuration, durationMs, sqlTag)
		c.pushSample(ctx, c.mi.sqlQueryCount, 1, sqlTag)
		c.pushSample(ctx, c.mi.sqlQueryFailed, 0, sqlTag)
		c.pushSample(ctx, c.mi.sqlRowsReturned, float64(count), sqlTag)
		return count, nil
	}

	globalFileMutex.Lock()
	defer globalFileMutex.Unlock()

	var file *os.File
	if isAppend {
		file, err = os.OpenFile(outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		file, err = os.Create(outputFile)
	}
	if err != nil {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return failSQL(fmt.Errorf("failed to open/create CSV file: %w", err))
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return failSQL(fmt.Errorf("stat file: %w", err))
	}
	isNewFile := fileInfo.Size() == 0

	if isNewFile && withBom {
		if _, err := file.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
			return failSQL(fmt.Errorf("failed to write BOM: %w", err))
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
			c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
			return failSQL(fmt.Errorf("failed to write headers: %w", err))
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
			c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
			return count, fmt.Errorf("failed to write row: %w", err)
		}
		count++
	}

	if err := rows.Err(); err != nil {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return count, fmt.Errorf("error during row iteration: %w", err)
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		c.pushSample(ctx, c.mi.csvWriteFailed, 1, fileTag)
		return count, fmt.Errorf("CSV flush error: %w", err)
	}

	// 🔹 Успешные метрики SQL + CSV
	durationMs := float64(time.Since(startTime).Milliseconds())
	c.pushSample(ctx, c.mi.sqlQueryDuration, durationMs, sqlTag)
	c.pushSample(ctx, c.mi.sqlQueryCount, 1, sqlTag)
	c.pushSample(ctx, c.mi.sqlQueryFailed, 0, sqlTag)
	c.pushSample(ctx, c.mi.sqlRowsReturned, float64(count), sqlTag)
	c.pushSample(ctx, c.mi.csvWriteDuration, durationMs, fileTag)
	c.pushSample(ctx, c.mi.csvRowsWritten, float64(count), fileTag)
	c.pushSample(ctx, c.mi.csvWriteFailed, 0, fileTag)

	return count, nil
}
package csvexporter

import (
    "encoding/csv"
    "os"
    "github.com/grafana/xk6-sql/sql"
)

func ExportToCSV(rows []sql.Row, filename string, delimiter rune) error {
    f, err := os.Create(filename)
    if err != return err
    defer f.Close()
    
    w := csv.NewWriter(f)
    w.Comma = delimiter
    return w.Flush()
}
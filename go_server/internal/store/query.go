package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/songbo/uptime-kuma/go_server/internal/logx"
)

// Row 是查询结果的通用行，键为列名。
type Row map[string]any

// Str 取字符串字段（NULL 返回 ""）。
func (r Row) Str(key string) string {
	switch v := r[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

// Int 取整数字段（NULL 或非法返回 0）。
func (r Row) Int(key string) int64 {
	switch v := r[key].(type) {
	case nil:
		return 0
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case []byte:
		var n int64
		_, _ = fmt.Sscan(string(v), &n)
		return n
	case string:
		var n int64
		_, _ = fmt.Sscan(v, &n)
		return n
	default:
		return 0
	}
}

// Float 取浮点字段。
func (r Row) Float(key string) float64 {
	switch v := r[key].(type) {
	case nil:
		return 0
	case float64:
		return v
	case int64:
		return float64(v)
	case []byte:
		var f float64
		_, _ = fmt.Sscan(string(v), &f)
		return f
	case string:
		var f float64
		_, _ = fmt.Sscan(v, &f)
		return f
	default:
		return 0
	}
}

// Bool 取布尔字段。
func (r Row) Bool(key string) bool {
	switch v := r[key].(type) {
	case nil:
		return false
	case bool:
		return v
	case int64:
		return v != 0
	case []byte:
		s := strings.ToLower(string(v))
		return s == "1" || s == "true"
	case string:
		s := strings.ToLower(v)
		return s == "1" || s == "true"
	default:
		return false
	}
}

// Insert 插入一行，返回自增主键。
func (d *DB) Insert(ctx context.Context, table string, values map[string]any) (int64, error) {
	cols, vals := d.filterColumns(ctx, table, values)
	if len(cols) == 0 {
		return 0, fmt.Errorf("插入 %s 失败：没有可用列", table)
	}

	quoted := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "[" + c + "]"
		marks[i] = "?"
	}

	q := fmt.Sprintf("INSERT INTO [%s] (%s) VALUES (%s)",
		table, strings.Join(quoted, ","), strings.Join(marks, ","))

	res, err := d.db.ExecContext(ctx, q, vals...)
	if err != nil {
		return 0, fmt.Errorf("插入 %s 失败: %w", table, err)
	}
	return res.LastInsertId()
}

// Update 按主键更新一行。
func (d *DB) Update(ctx context.Context, table string, id int64, values map[string]any) error {
	cols, vals := d.filterColumns(ctx, table, values)
	if len(cols) == 0 {
		return nil
	}

	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = "[" + c + "] = ?"
	}
	vals = append(vals, id)

	q := fmt.Sprintf("UPDATE [%s] SET %s WHERE id = ?", table, strings.Join(sets, ","))
	if _, err := d.db.ExecContext(ctx, q, vals...); err != nil {
		return fmt.Errorf("更新 %s(%d) 失败: %w", table, id, err)
	}
	return nil
}

// Delete 按主键删除。
func (d *DB) Delete(ctx context.Context, table string, id int64) error {
	q := fmt.Sprintf("DELETE FROM [%s] WHERE id = ?", table)
	if _, err := d.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("删除 %s(%d) 失败: %w", table, id, err)
	}
	return nil
}

// DeleteWhere 按条件删除。
func (d *DB) DeleteWhere(ctx context.Context, table, where string, args ...any) error {
	q := fmt.Sprintf("DELETE FROM [%s] WHERE %s", table, where)
	if _, err := d.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("按条件删除 %s 失败: %w", table, err)
	}
	return nil
}

// Query 执行查询并返回通用行集合。
func (d *DB) Query(ctx context.Context, q string, args ...any) ([]Row, error) {
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// QueryOne 返回第一行，无数据返回 nil。
func (d *DB) QueryOne(ctx context.Context, q string, args ...any) (Row, error) {
	rows, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// FindByID 按主键取一行。
func (d *DB) FindByID(ctx context.Context, table string, id int64) (Row, error) {
	q := fmt.Sprintf("SELECT * FROM [%s] WHERE id = ?", table)
	return d.QueryOne(ctx, q, id)
}

// FindAll 取全表。
func (d *DB) FindAll(ctx context.Context, table string, orderBy string) ([]Row, error) {
	q := fmt.Sprintf("SELECT * FROM [%s]", table)
	if orderBy != "" {
		q += " ORDER BY " + orderBy
	}
	return d.Query(ctx, q)
}

// Count 统计行数。
func (d *DB) Count(ctx context.Context, table, where string, args ...any) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) AS n FROM [%s]", table)
	if where != "" {
		q += " WHERE " + where
	}
	row, err := d.QueryOne(ctx, q, args...)
	if err != nil || row == nil {
		return 0, err
	}
	return row.Int("n"), nil
}

// Exists 判断是否存在满足条件的行。
func (d *DB) Exists(ctx context.Context, table, where string, args ...any) (bool, error) {
	n, err := d.Count(ctx, table, where, args...)
	return n > 0, err
}

// Exec 直接执行写语句。
func (d *DB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, q, args...)
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out := make([]Row, 0, 16)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}

		r := make(Row, len(cols))
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte:
				r[c] = string(v)
			case time.Time:
				// 部分驱动会把 DATETIME 列自动解析成时间对象，
				// 这里统一成官方格式，保证前端拿到的永远是字符串
				r[c] = FormatTime(v)
			default:
				r[c] = vals[i]
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Tx 在事务中执行 fn。
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			logx.Warnf("store", "回滚事务失败: %v", rbErr)
		}
		return err
	}
	return tx.Commit()
}

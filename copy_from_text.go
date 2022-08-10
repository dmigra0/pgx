package pgx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgio"
)

// CopyFromRows returns a CopyFromSource interface over the provided rows slice
// making it usable by *Conn.CopyFrom.
type copyFromText struct {
	conn          *Conn
	tableName     Identifier
	columnNames   []string
	rowSrc        CopyFromSource
	readerErrChan chan error
}

func (ct *copyFromText) run(ctx context.Context) (int64, error) {
	quotedTableName := ct.tableName.Sanitize()
	cbuf := &bytes.Buffer{}
	for i, cn := range ct.columnNames {
		if i != 0 {
			cbuf.WriteString(", ")
		}
		cbuf.WriteString(quoteIdentifier(cn))
	}
	quotedColumnNames := cbuf.String()

	sd, err := ct.conn.Prepare(ctx, "", fmt.Sprintf("select %s from %s", quotedColumnNames, quotedTableName))
	if err != nil {
		return 0, err
	}

	r, w := io.Pipe()
	doneChan := make(chan struct{})

	go func() {
		defer close(doneChan)

		// Purposely NOT using defer w.Close(). See https://github.com/golang/go/issues/24283.
		buf := ct.conn.wbuf

		buf = append(buf, "PGCOPY\n\377\r\n\000"...)
		buf = pgio.AppendInt32(buf, 0)
		buf = pgio.AppendInt32(buf, 0)

		moreRows := true
		for moreRows {
			var err error
			moreRows, buf, err = ct.buildCopyBuf(buf, sd)
			if err != nil {
				w.CloseWithError(err)
				return
			}

			if ct.rowSrc.Err() != nil {
				w.CloseWithError(ct.rowSrc.Err())
				return
			}

			if len(buf) > 0 {
				_, err = w.Write(buf)
				if err != nil {
					w.Close()
					return
				}
			}

			buf = buf[:0]
		}

		w.Close()
	}()

	startTime := time.Now()

	commandTag, err := ct.conn.pgConn.CopyFrom(ctx, r, fmt.Sprintf("copy %s ( %s ) from stdin;", quotedTableName, quotedColumnNames))

	r.Close()
	<-doneChan

	rowsAffected := commandTag.RowsAffected()
	endTime := time.Now()
	if err == nil {
		if ct.conn.shouldLog(LogLevelInfo) {
			ct.conn.log(ctx, LogLevelInfo, "CopyFrom", map[string]interface{}{"tableName": ct.tableName, "columnNames": ct.columnNames, "time": endTime.Sub(startTime), "rowCount": rowsAffected})
		}
	} else if ct.conn.shouldLog(LogLevelError) {
		ct.conn.log(ctx, LogLevelError, "CopyFrom", map[string]interface{}{"err": err, "tableName": ct.tableName, "columnNames": ct.columnNames, "time": endTime.Sub(startTime)})
	}

	return rowsAffected, err
}

func (ct *copyFromText) buildCopyBuf(buf []byte, sd *pgconn.StatementDescription) (bool, []byte, error) {

	for ct.rowSrc.Next() {
		values, err := ct.rowSrc.Values()
		if err != nil {
			return false, nil, err
		}
		if len(values) != len(ct.columnNames) {
			return false, nil, fmt.Errorf("expected %d values, got %d values", len(ct.columnNames), len(values))
		}

		buf = pgio.AppendInt16(buf, int16(len(ct.columnNames)))
		for i, val := range values {
			buf, err = encodePreparedStatementArgument(ct.conn.connInfo, buf, sd.Fields[i].DataTypeOID, val)
			if err != nil {
				return false, nil, err
			}
		}

		if len(buf) > 65536 {
			return true, buf, nil
		}
	}

	return false, buf, nil
}

// CopyFrom uses the PostgreSQL copy protocol to perform bulk data insertion.
// It returns the number of rows copied and an error.
//
// CopyFrom requires all values use the binary format. Almost all types
// implemented by pgx use the binary format by default. Types implementing
// Encoder can only be used if they encode to the binary format.
func (c *Conn) CopyFromText(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error) {
	ct := &copyFromText{
		conn:          c,
		tableName:     tableName,
		columnNames:   columnNames,
		rowSrc:        rowSrc,
		readerErrChan: make(chan error),
	}

	return ct.run(ctx)
}

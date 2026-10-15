package controller

import (
	"bytes"
	"net/http"

	"github.com/gin-gonic/gin"
)

// taskResponseWriter holds the adaptor's JSON response until the accepted task
// has been persisted. A database failure must not leave an unqueryable task ID
// committed as an HTTP success. Task submissions do not stream.
type taskResponseWriter struct {
	gin.ResponseWriter
	header  http.Header
	body    bytes.Buffer
	status  int
	written bool
}

func (w *taskResponseWriter) Header() http.Header { return w.header }
func (w *taskResponseWriter) Status() int         { return w.status }
func (w *taskResponseWriter) Written() bool       { return w.written }
func (w *taskResponseWriter) Size() int {
	if !w.written {
		return -1
	}
	return w.body.Len()
}
func (w *taskResponseWriter) WriteHeader(status int) {
	if status > 0 && !w.written {
		w.status = status
	}
}
func (w *taskResponseWriter) WriteHeaderNow() { w.written = true }
func (w *taskResponseWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	return w.body.Write(data)
}
func (w *taskResponseWriter) WriteString(data string) (int, error) {
	w.WriteHeaderNow()
	return w.body.WriteString(data)
}
func (w *taskResponseWriter) Flush() { w.WriteHeaderNow() }

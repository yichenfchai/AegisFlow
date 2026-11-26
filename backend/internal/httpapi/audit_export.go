package httpapi

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// auditCSVHeader is the fixed export column order. The columns mirror the audit
// JSON fields except requestId: the export is a report for humans, and the
// request id already travels on every API response. No column carries a
// credential, token, cookie or request body; detail is the same redacted field
// the audit page renders.
const auditCSVHeader = "id,createdAt,action,objectType,objectId,result,actor,detail"

// auditExportLimit bounds one export. It is a variable only so tests can shrink
// it without inserting ten thousand rows; production always uses the documented
// cap in the auth package.
var auditExportLimit = auth.AuditExportMaxRows

// exportAuditCSV returns the filtered audit trail as RFC 4180 CSV (admin only;
// the route sits in the authenticated admin group). The filter semantics and the
// from/to formats are exactly the audit list's. At most auditExportLimit rows are
// written; when more match, the X-Audit-Truncated response header is set so the
// client can tell the user to narrow the filters. No rows still yields the
// header row, so the file is always a valid CSV document.
func (s *Server) exportAuditCSV(c *gin.Context) {
	filter, ok := s.auditFilter(c)
	if !ok {
		return
	}
	entries, truncated, err := s.auth.ExportAudit(c.Request.Context(), filter, auditExportLimit)
	if err != nil {
		s.authError(c, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="audit-`+time.Now().UTC().Format("20060102-150405")+`.csv"`)
	if truncated {
		c.Header("X-Audit-Truncated", "true")
	}
	c.Data(http.StatusOK, "text/csv; charset=utf-8", auditCSV(entries))
}

// auditCSV renders the exported rows. createdAt is RFC3339 in UTC so the report
// is unambiguous regardless of the viewer's timezone; every field goes through
// csvField (quotes, commas and newlines are escaped per RFC 4180) and records end
// with CRLF as the format requires.
func auditCSV(entries []auth.AuditEntry) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(auditCSVHeader)
	buffer.WriteString("\r\n")
	for _, entry := range entries {
		actor := entry.ActorUsername
		if actor == "" {
			actor = entry.ActorID
		}
		writeCSVRecord(&buffer, []string{
			entry.ID,
			time.Unix(entry.CreatedAt, 0).UTC().Format(time.RFC3339),
			entry.Action,
			entry.ObjectType,
			entry.ObjectID,
			entry.Result,
			actor,
			entry.Detail,
		})
	}
	return buffer.Bytes()
}

// csvField escapes one RFC 4180 field. A field containing a comma, a double
// quote, CR or LF is wrapped in double quotes and every embedded quote is
// doubled; everything else is written verbatim.
func csvField(value string) string {
	if !strings.ContainsAny(value, ",\"\r\n") {
		return value
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// writeCSVRecord writes one row; fields are joined with commas and the record
// ends with CRLF.
func writeCSVRecord(buffer *bytes.Buffer, fields []string) {
	for i, field := range fields {
		if i > 0 {
			buffer.WriteByte(',')
		}
		buffer.WriteString(csvField(field))
	}
	buffer.WriteString("\r\n")
}

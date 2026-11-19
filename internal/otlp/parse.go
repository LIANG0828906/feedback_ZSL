package otlp

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"quota-coordinator/internal/model"
)

var (
	ErrUnsupportedContentType = errors.New("unsupported content type")
	ErrNoLogRecords           = errors.New("request contains no log records")
	ErrRecordWithoutTenant    = errors.New("log record has no tenant identifier")
)

const maxEncodedRecordBytes = 16 << 20

type ParseOptions struct {
	HeaderBatchID string
	Now           time.Time
}

func DecodeBody(body []byte, contentEncoding string) ([]byte, error) {
	if !strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("open gzip body: %w", err)
	}
	defer zr.Close()
	limited := io.LimitReader(zr, maxEncodedRecordBytes+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read gzip body: %w", err)
	}
	if len(out) > maxEncodedRecordBytes {
		return nil, errors.New("decoded body is too large")
	}
	return out, nil
}

func Parse(body []byte, contentType string, opts ParseOptions) ([]model.Record, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	switch ct {
	case "application/x-protobuf", "application/protobuf", "application/octet-stream":
		return parseProtobuf(body, opts)
	case "application/json", "application/x-ndjson", "application/jsonlines", "text/json", "text/plain", "":
		return parseJSONLike(body, opts)
	default:
		if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
			return parseJSONLike(body, opts)
		}
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedContentType, contentType)
	}
}

func parseProtobuf(body []byte, opts ParseOptions) ([]model.Record, error) {
	req := &collectorlogspb.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(body, req); err != nil {
		return nil, fmt.Errorf("decode OTLP protobuf: %w", err)
	}
	var records []model.Record
	for _, rl := range req.GetResourceLogs() {
		resource := attributesToMap(rl.GetResource().GetAttributes())
		tenant := findString(resource, tenantKeys...)
		for _, sl := range rl.GetScopeLogs() {
			scope := map[string]any{
				"name":    sl.GetScope().GetName(),
				"version": sl.GetScope().GetVersion(),
			}
			for _, lr := range sl.GetLogRecords() {
				attrs := attributesToMap(lr.GetAttributes())
				recordTenant := findString(attrs, tenantKeys...)
				if recordTenant == "" {
					recordTenant = tenant
				}
				if recordTenant == "" {
					return nil, ErrRecordWithoutTenant
				}
				recordID := findString(attrs, recordIDKeys...)
				if recordID == "" {
					recordID = shortHash(
						recordTenant,
						rl.GetSchemaUrl(),
						strconv.FormatUint(lr.GetTimeUnixNano(), 10),
						strconv.FormatUint(lr.GetObservedTimeUnixNano(), 10),
						lr.GetBody().String(),
					)
				}
				batchID := findString(attrs, batchIDKeys...)
				if batchID == "" {
					batchID = opts.HeaderBatchID
				}
				ts := time.Unix(0, int64(lr.GetTimeUnixNano())).UTC()
				if lr.GetTimeUnixNano() == 0 {
					ts = opts.Now
				}
				data := map[string]any{
					"tenant_id":          recordTenant,
					"record_id":          recordID,
					"timestamp":          ts.Format(time.RFC3339Nano),
					"observed_timestamp": unixNanoString(lr.GetObservedTimeUnixNano()),
					"severity_number":    int64(lr.GetSeverityNumber()),
					"severity_text":      lr.GetSeverityText(),
					"body":               anyValueToGo(lr.GetBody()),
					"attributes":         attrs,
					"resource":           resource,
					"scope":              scope,
				}
				raw, err := marshalRecordData(data)
				if err != nil {
					return nil, err
				}
				records = append(records, model.Record{
					RecordID:  recordID,
					TenantID:  recordTenant,
					BatchID:   batchID,
					Timestamp: ts,
					Data:      raw,
					Bytes:     int64(len(raw)),
				})
			}
		}
	}
	if len(records) == 0 {
		return nil, ErrNoLogRecords
	}
	return records, nil
}

func parseJSONLike(body []byte, opts ParseOptions) ([]model.Record, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var records []model.Record
	for {
		var value any
		err := dec.Decode(&value)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode JSON body: %w", err)
		}
		parsed, err := recordsFromJSONValue(value, opts)
		if err != nil {
			return nil, err
		}
		records = append(records, parsed...)
	}
	if len(records) == 0 {
		return nil, ErrNoLogRecords
	}
	return records, nil
}

func recordsFromJSONValue(value any, opts ParseOptions) ([]model.Record, error) {
	switch v := value.(type) {
	case []any:
		var out []model.Record
		for _, child := range v {
			parsed, err := recordsFromJSONValue(child, opts)
			if err != nil {
				return nil, err
			}
			out = append(out, parsed...)
		}
		return out, nil
	case map[string]any:
		if resourceLogs, ok := v["resourceLogs"].([]any); ok {
			return recordsFromOTLPJSON(resourceLogs, opts)
		}
		if resourceLogs, ok := v["resource_logs"].([]any); ok {
			return recordsFromOTLPJSON(resourceLogs, opts)
		}
		if records, ok := v["records"].([]any); ok {
			return recordsFromCustomBatch(v, records, opts)
		}
		if events, ok := v["events"].([]any); ok {
			return recordsFromCustomBatch(v, events, opts)
		}
		if inner, ok := v["event"].(map[string]any); ok {
			return recordsFromJSONValue(inner, opts)
		}
		record, err := recordFromEvent(v, opts)
		if err != nil {
			return nil, err
		}
		return []model.Record{record}, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported JSON log value %T", value)
	}
}

func recordsFromOTLPJSON(resourceLogs []any, opts ParseOptions) ([]model.Record, error) {
	var out []model.Record
	for _, rlv := range resourceLogs {
		rl, _ := rlv.(map[string]any)
		resource := attributesFromAny(resourceAttributesValue(rl["resource"]))
		resourceTenant := findString(resource, tenantKeys...)
		scopeLogs, _ := rl["scopeLogs"].([]any)
		if scopeLogs == nil {
			scopeLogs, _ = rl["scope_logs"].([]any)
		}
		for _, slv := range scopeLogs {
			sl, _ := slv.(map[string]any)
			logRecords, _ := sl["logRecords"].([]any)
			if logRecords == nil {
				logRecords, _ = sl["log_records"].([]any)
			}
			for _, lrv := range logRecords {
				lr, ok := lrv.(map[string]any)
				if !ok {
					continue
				}
				attrs := attributesFromAny(lr["attributes"])
				tenant := findString(attrs, tenantKeys...)
				if tenant == "" {
					tenant = resourceTenant
				}
				if tenant == "" {
					return nil, ErrRecordWithoutTenant
				}
				recordID := findString(attrs, recordIDKeys...)
				if recordID == "" {
					recordID = findString(lr, recordIDKeys...)
				}
				rawForID, _ := json.Marshal(lr)
				if recordID == "" {
					recordID = shortHash(tenant, string(rawForID))
				}
				batchID := findString(attrs, batchIDKeys...)
				if batchID == "" {
					batchID = findString(lr, batchIDKeys...)
				}
				if batchID == "" {
					batchID = opts.HeaderBatchID
				}
				ts := parseTimestamp(lr["timeUnixNano"], opts.Now)
				if ts.IsZero() {
					ts = parseTimestamp(lr["time_unix_nano"], opts.Now)
				}
				if ts.IsZero() {
					ts = parseTimestamp(lr["timestamp"], opts.Now)
				}
				data := map[string]any{
					"tenant_id":          tenant,
					"record_id":          recordID,
					"timestamp":          ts.Format(time.RFC3339Nano),
					"observed_timestamp": stringValue(lr["observedTimeUnixNano"]),
					"severity_number":    lr["severityNumber"],
					"severity_text":      stringValue(lr["severityText"]),
					"body":               unwrapOTLPJSONValue(lr["body"], 0),
					"attributes":         attrs,
					"resource":           resource,
					"scope":              sl["scope"],
				}
				raw, err := marshalRecordData(data)
				if err != nil {
					return nil, err
				}
				out = append(out, model.Record{
					RecordID:  recordID,
					TenantID:  tenant,
					BatchID:   batchID,
					Timestamp: ts,
					Data:      raw,
					Bytes:     int64(len(raw)),
				})
			}
		}
	}
	if len(out) == 0 {
		return nil, ErrNoLogRecords
	}
	return out, nil
}

func resourceAttributesValue(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if attrs, ok := object["attributes"]; ok {
		return attrs
	}
	return value
}

func recordsFromCustomBatch(batch map[string]any, values []any, opts ParseOptions) ([]model.Record, error) {
	tenant := firstNonEmpty(
		findString(batch, tenantKeys...),
		findNestedString(batch, []string{"resource", "attributes"}, tenantKeys...),
	)
	batchID := firstNonEmpty(findString(batch, batchIDKeys...), opts.HeaderBatchID)
	if tenant == "" {
		return nil, ErrRecordWithoutTenant
	}
	out := make([]model.Record, 0, len(values))
	for i, value := range values {
		event, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("record %d is not an object", i)
		}
		record, err := recordFromEvent(event, ParseOptions{HeaderBatchID: batchID, Now: opts.Now})
		if err != nil {
			return nil, err
		}
		if record.TenantID == "" {
			record.TenantID = tenant
		}
		if record.BatchID == "" {
			record.BatchID = batchID
		}
		record.Data, record.Bytes, err = addNormalizedFields(record.Data, record.TenantID, record.RecordID, record.Timestamp)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

func recordFromEvent(event map[string]any, opts ParseOptions) (model.Record, error) {
	tenant := firstNonEmpty(
		findString(event, tenantKeys...),
		findNestedString(event, []string{"attributes"}, tenantKeys...),
		findNestedString(event, []string{"resource", "attributes"}, tenantKeys...),
		findNestedString(event, []string{"resources"}, tenantKeys...),
		findNestedString(event, []string{"metadata"}, tenantKeys...),
	)
	if tenant == "" {
		return model.Record{}, ErrRecordWithoutTenant
	}
	recordID := firstNonEmpty(
		findString(event, recordIDKeys...),
		findNestedString(event, []string{"attributes"}, recordIDKeys...),
	)
	rawForID, _ := json.Marshal(event)
	if recordID == "" {
		recordID = shortHash(tenant, string(rawForID))
	}
	batchID := firstNonEmpty(
		findString(event, batchIDKeys...),
		findNestedString(event, []string{"attributes"}, batchIDKeys...),
		opts.HeaderBatchID,
	)
	ts := parseTimestamp(event["timestamp"], opts.Now)
	if ts.IsZero() {
		ts = parseTimestamp(event["time"], opts.Now)
	}
	if ts.IsZero() {
		ts = parseTimestamp(event["timeUnixNano"], opts.Now)
	}
	if ts.IsZero() {
		ts = parseTimestamp(event["time_unix_nano"], opts.Now)
	}
	if ts.IsZero() {
		ts = opts.Now
	}
	raw, bytes, err := addNormalizedFields(rawForID, tenant, recordID, ts)
	if err != nil {
		return model.Record{}, err
	}
	return model.Record{RecordID: recordID, TenantID: tenant, BatchID: batchID, Timestamp: ts, Data: raw, Bytes: bytes}, nil
}

func addNormalizedFields(raw json.RawMessage, tenant, recordID string, ts time.Time) (json.RawMessage, int64, error) {
	var event map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&event); err != nil {
		return nil, 0, err
	}
	event["tenant_id"] = tenant
	event["record_id"] = recordID
	if _, ok := event["timestamp"]; !ok {
		event["timestamp"] = ts.UTC().Format(time.RFC3339Nano)
	}
	out, err := marshalRecordData(event)
	return out, int64(len(out)), err
}

func marshalRecordData(data map[string]any) (json.RawMessage, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized record: %w", err)
	}
	return raw, nil
}

func attributesToMap(attrs []*commonpb.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		if attr == nil || attr.GetKey() == "" {
			continue
		}
		out[attr.GetKey()] = anyValueToGo(attr.GetValue())
	}
	return out
}

func anyValueToGo(v *commonpb.AnyValue) any {
	if v == nil {
		return nil
	}
	switch value := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return value.StringValue
	case *commonpb.AnyValue_BoolValue:
		return value.BoolValue
	case *commonpb.AnyValue_IntValue:
		return value.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return value.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(value.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		values := value.ArrayValue.GetValues()
		out := make([]any, 0, len(values))
		for _, child := range values {
			out = append(out, anyValueToGo(child))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		return attributesToMap(value.KvlistValue.GetValues())
	default:
		return nil
	}
}

func attributesFromAny(value any) map[string]any {
	switch attrs := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return attrs
	case []any:
		out := map[string]any{}
		for _, item := range attrs {
			kv, ok := item.(map[string]any)
			if !ok {
				continue
			}
			key := stringValue(kv["key"])
			if key == "" {
				continue
			}
			out[key] = unwrapOTLPJSONValue(kv["value"], 0)
		}
		return out
	default:
		return map[string]any{}
	}
}

func unwrapOTLPJSONValue(value any, depth int) any {
	if depth > 8 {
		return value
	}
	obj, ok := value.(map[string]any)
	if !ok || len(obj) != 1 {
		return value
	}
	for key, child := range obj {
		switch key {
		case "stringValue", "string_value":
			return child
		case "boolValue", "bool_value":
			return child
		case "intValue", "int_value":
			return parseIntValue(child)
		case "doubleValue", "double_value":
			return child
		case "bytesValue", "bytes_value":
			return child
		case "arrayValue", "array_value":
			if arr, ok := child.(map[string]any); ok {
				values, _ := arr["values"].([]any)
				out := make([]any, 0, len(values))
				for _, item := range values {
					out = append(out, unwrapOTLPJSONValue(item, depth+1))
				}
				return out
			}
			return child
		case "kvlistValue", "kvlist_value":
			if kv, ok := child.(map[string]any); ok {
				return attributesFromAny(kv["values"])
			}
			return child
		}
	}
	return value
}

func parseIntValue(v any) any {
	s := stringValue(v)
	if s == "" {
		return v
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}

var tenantKeys = []string{"tenant.id", "tenant_id", "tenantId", "tenant", "log.tenant.id"}
var recordIDKeys = []string{"record.id", "record_id", "recordId", "log.record.id", "_record_id"}
var batchIDKeys = []string{"batch.id", "batch_id", "batchId", "_batch_id"}

func findString(value any, keys ...string) string {
	obj, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range keys {
		key = strings.ToLower(key)
		if direct, ok := obj[key]; ok {
			if s := scalarString(direct); s != "" {
				return s
			}
		}
		for actual, child := range obj {
			if strings.ToLower(actual) == key {
				if s := scalarString(child); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func findNestedString(value any, path []string, keys ...string) string {
	current := value
	for _, part := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = obj[part]
		if current == nil {
			current = obj[strings.ToLower(part)]
		}
	}
	return findString(current, keys...)
}

func scalarString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		if value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case uint64:
		return strconv.FormatUint(value, 10)
	case bool:
		return strconv.FormatBool(value)
	case map[string]any:
		return stringValue(unwrapOTLPJSONValue(value, 0))
	default:
		return ""
	}
}

func stringValue(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case json.Number:
		return value.String()
	default:
		raw, _ := json.Marshal(value)
		return string(raw)
	}
}

func parseTimestamp(v any, fallback time.Time) time.Time {
	switch value := v.(type) {
	case nil:
		return time.Time{}
	case json.Number:
		return timestampFromNumber(string(value))
	case string:
		s := strings.TrimSpace(value)
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
		return timestampFromNumber(s)
	case float64:
		return timestampFromNumber(strconv.FormatFloat(value, 'f', -1, 64))
	case int64:
		return timestampFromNumber(strconv.FormatInt(value, 10))
	case uint64:
		return timestampFromNumber(strconv.FormatUint(value, 10))
	default:
		_ = fallback
		return time.Time{}
	}
}

func timestampFromNumber(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch {
	case n > 1e18:
		return time.Unix(0, n).UTC()
	case n > 1e15:
		return time.Unix(0, n*1e3).UTC()
	case n > 1e12:
		return time.Unix(0, n*1e6).UTC()
	default:
		return time.Unix(n, 0).UTC()
	}
}

func unixNanoString(n uint64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func shortHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func GroupRecords(records []model.Record, requestHash string) []model.Batch {
	groups := make(map[string]*model.Batch)
	for _, record := range records {
		batchID := record.BatchID
		if batchID == "" {
			batchID = requestHash
		}
		key := record.TenantID + "\x00" + batchID
		group := groups[key]
		if group == nil {
			group = &model.Batch{TenantID: record.TenantID, BatchID: batchID}
			groups[key] = group
		}
		group.Records = append(group.Records, record)
		group.Count++
		group.Bytes += record.Bytes
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]model.Batch, 0, len(keys))
	for _, key := range keys {
		out = append(out, *groups[key])
	}
	return out
}

func RequestHash(headerBatchID string, records []model.Record) string {
	if headerBatchID != "" {
		return headerBatchID
	}
	h := sha256.New()
	for _, record := range records {
		_, _ = io.WriteString(h, record.TenantID)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, record.RecordID)
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(record.Data)
		_, _ = h.Write([]byte{0})
	}
	return "auto-" + hex.EncodeToString(h.Sum(nil))[:32]
}

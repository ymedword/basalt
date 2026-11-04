package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

// handleObservability 只承担管理只读分发；路由不声明caller scope/ingress。
// handleObservability dispatches management reads without caller scopes or ingress mappings.
func (s *Server) handleObservability(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	reader := s.opts.Observability
	if reader == nil {
		s.observationResult(w, nil, errors.New("observability read port missing"))
		return
	}
	if !reader.ObservationEnabled() {
		s.writeError(w, http.StatusServiceUnavailable, codeNotConfigured, "可观测功能未启用")
		return
	}
	var data any
	var err error
	var p url.Values
	switch strings.TrimPrefix(r.URL.Path, "/api/v1/observability/") {
	case "overview":
		p, err = observationParams(r, "from", "to", "agent", "limit")
		if err == nil {
			var q observability.ReadWindow
			q, err = observationWindow(p, s.opts.Now(), 100)
			if err == nil && p.Get("agent") != "" {
				err = (observability.Anchor{Kind: observability.AnchorRun, Value: p.Get("agent")}).Validate()
			}
			if err == nil {
				data, err = reader.Overview(r.Context(), observability.OverviewQuery{From: q.From, To: q.To, Agent: p.Get("agent"), Limit: q.Limit})
			}
		}
	case "timeline":
		p, err = observationParams(r, "run_id", "request_id", "task_id", "flow_task_id", "operation_id", "ingest_job_id", "session_id", "from", "to", "sources", "limit")
		if err == nil {
			q := observability.TimelineQuery{Limit: 200, Sources: observationSources(p.Get("sources"))}
			q.Anchor, err = observationAnchor(p)
			if err == nil && p.Get("from") != "" {
				q.From, err = time.Parse(time.RFC3339Nano, p.Get("from"))
			}
			if err == nil && p.Get("to") != "" {
				q.To, err = time.Parse(time.RFC3339Nano, p.Get("to"))
			}
			if err == nil && p.Get("limit") != "" {
				q.Limit, err = strconv.Atoi(p.Get("limit"))
				if q.Limit < 1 {
					err = observability.ErrInvalidQuery
				}
			}
			if err == nil {
				err = q.Validate()
			}
			if err == nil {
				data, err = reader.Timeline(r.Context(), q)
			} else {
				err = observability.ErrInvalidQuery
			}
		}
	case "context":
		p, err = observationParams(r, "run_id")
		if err == nil {
			err = (observability.Anchor{Kind: observability.AnchorRun, Value: p.Get("run_id")}).Validate()
		}
		if err == nil {
			data, err = reader.Context(r.Context(), p.Get("run_id"))
		}
	case "metrics":
		p, err = observationParams(r, "window")
		window := 15 * time.Minute
		if err == nil && p.Get("window") != "" {
			switch p.Get("window") {
			case "5m":
				window = 5 * time.Minute
			case "15m":
			case "1h":
				window = time.Hour
			case "24h":
				window = 24 * time.Hour
			default:
				err = observability.ErrInvalidQuery
			}
		}
		if err == nil {
			data, err = reader.MetricsSnapshot(r.Context(), window)
		}
	case "alerts", "cleanup":
		p, err = observationParams(r, "from", "to", "limit")
		if err == nil {
			var q observability.ReadWindow
			q, err = observationWindow(p, s.opts.Now(), 1000)
			if err == nil {
				if strings.HasSuffix(r.URL.Path, "/alerts") {
					data, err = reader.AlertStatus(r.Context(), q)
				} else {
					data, err = reader.CleanupStatus(r.Context(), q)
				}
			}
		}
	case "export":
		_, err = observationParams(r)
		if err == nil {
			data, err = reader.ExportStatus(r.Context())
		}
	default:
		err = observability.ErrInvalidQuery
	}
	s.observationResult(w, data, err)
}

func observationParams(r *http.Request, allowed ...string) (url.Values, error) {
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, observability.ErrInvalidQuery
	}
	for key, values := range params {
		ok := false
		for _, k := range allowed {
			if key == k {
				ok = true
				break
			}
		}
		if !ok || len(values) != 1 || values[0] == "" {
			return nil, observability.ErrInvalidQuery
		}
	}
	return params, nil
}

func observationWindow(p url.Values, now time.Time, maxLimit int) (observability.ReadWindow, error) {
	q := observability.ReadWindow{To: now.UTC(), Limit: min(100, maxLimit)}
	var err error
	if v := p.Get("to"); v != "" {
		q.To, err = time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return q, observability.ErrInvalidQuery
		}
	}
	q.From = q.To.Add(-24 * time.Hour)
	if v := p.Get("from"); v != "" {
		q.From, err = time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return q, observability.ErrInvalidQuery
		}
	}
	if v := p.Get("limit"); v != "" {
		q.Limit, err = strconv.Atoi(v)
		if err != nil {
			return q, observability.ErrInvalidQuery
		}
	}
	if q.Limit > maxLimit {
		return q, observability.ErrInvalidQuery
	}
	return q, q.Validate()
}

func observationAnchor(p url.Values) (observability.Anchor, error) {
	a := observability.Anchor{}
	count := 0
	for _, entry := range []struct {
		key  string
		kind observability.AnchorKind
	}{{"run_id", observability.AnchorRun}, {"request_id", observability.AnchorRequest}, {"task_id", observability.AnchorTask}, {"flow_task_id", observability.AnchorFlowTask}, {"operation_id", observability.AnchorOperation}, {"ingest_job_id", observability.AnchorIngest}, {"session_id", observability.AnchorSession}} {
		if id := p.Get(entry.key); id != "" {
			a = observability.Anchor{Kind: entry.kind, Value: id}
			count++
		}
	}
	if count != 1 {
		return a, observability.ErrInvalidQuery
	}
	return a, a.Validate()
}

func (s *Server) observationResult(w http.ResponseWriter, data any, err error) {
	if err == nil {
		s.writeOK(w, data)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, observability.ErrInvalidQuery) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "观测查询参数无效")
		return
	}
	if errors.Is(err, core.ErrRunNotFound) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "运行记录不存在")
		return
	}
	s.logger().Error("Observability read failed", "error_kind", "source_unavailable")
	s.writeError(w, http.StatusInternalServerError, codeInternal, "观测数据读取失败")
}

// observationSources 是纯解析器；具体允许集合由TimelineQuery再次校验。
// observationSources parses a fixed list; TimelineQuery validates the source set again.
func observationSources(value string) []observability.SourceKind {
	if value == "" {
		return nil
	}
	out := []observability.SourceKind{}
	for _, v := range strings.Split(value, ",") {
		out = append(out, observability.SourceKind(v))
	}
	return out
}

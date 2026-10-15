package volcvideo

import (
	"bytes"
	"math"
	"strconv"

	"github.com/QuantumNous/new-api/common"
)

// submitResponse is the Volcengine-native submit response:
//
//	{"id":"vid_x","task_id":"vid_x","object":"video","model":"...","status":"queued","progress":0,"created_at":...}
//
// task_id and id carry the same value upstream; ParseResponse prefers
// task_id and falls back to id for forward compatibility.
type submitResponse struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// pollEnvelope is the Volcengine-native polling envelope:
//
//	{"code":"success","message":"","data":{"task_id":"...","status":"SUCCESS", ...}}
//
// A not-found task comes back as HTTP 400 with
// {"code":"task_not_exist","message":"task_not_exist","data":null}.
type pollEnvelope struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Data    pollData `json:"data"`
}

type pollData struct {
	Status      string     `json:"status"`
	Progress    string     `json:"progress"`
	ResultURL   string     `json:"result_url"`
	FailReason  string     `json:"fail_reason"`
	TotalTokens tokenCount `json:"total_tokens"`
}

// tokenCount decodes the upstream total_tokens tolerantly: a JSON number
// (integral or float) or a numeric string. Anything else, a negative value,
// or a value above math.MaxInt32 becomes 0 so a malformed count can never
// become an absurd billing quantity.
type tokenCount int

func (t *tokenCount) UnmarshalJSON(data []byte) error {
	*t = 0
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := common.Unmarshal(data, &text); err != nil {
			return nil
		}
		data = bytes.TrimSpace([]byte(text))
	}
	value, err := strconv.ParseFloat(string(data), 64)
	if err != nil || math.IsNaN(value) || value < 0 || value > math.MaxInt32 {
		return nil
	}
	*t = tokenCount(value)
	return nil
}

func (t tokenCount) Int() int { return int(t) }

package volcvideo

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
	Status      string `json:"status"`
	Progress    string `json:"progress"`
	ResultURL   string `json:"result_url"`
	FailReason  string `json:"fail_reason"`
	TotalTokens int    `json:"total_tokens"`
}

package server

import (
	"net/http"
	"strings"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
)

// modelInfo 是 `/v1/models` 里的一条。
//
// 为什么需要它：本站有**两条上游通路**，同一件事的价格和风险完全不同 ——
// 网页会话有免费档但有 Turnstile 闸门；官方 REST 没有闸门但每单都扣积分。
// 调用方要能在**发请求之前**看清"这个模型走哪条、要不要钱"，而不是试出来。
type modelInfo struct {
	ID      string
	Channel string // web | official | both
	// WebReady / OfficialReady 表示**本服务**是否真能跑通，而不只是上游有这个名字。
	// 这两个字段是给调用方看的实话：`channel: both` 但 web_ready=false 就是"网页那条别试"。
	WebReady      bool
	OfficialReady bool
	Free          bool // 是否存在免费窗口
	// Credits5s 是 5 秒的参考价（积分）。nil 表示没实测过，别瞎填。
	Credits5s   any
	Resolutions []string
	Durations   []string
	Note        string
}

// modelCatalog 是**实测过**的清单，不是站点 UI 上抄的。
//
// 数据来源：
//   - 网页端 procedure 是否存在：未鉴权 401(存在) / 404(不存在)，见 docs/evidence/api-surface.md
//   - 官方价目：`/api/v1/quote/{model}` 实拉
//   - 免费档：只有 MiniMax H3 的 480p/720p + turbo + ≤10s（实测余额不变）
var modelCatalog = []modelInfo{
	{
		ID: "minimaxH3", Channel: "both", WebReady: true, OfficialReady: true, Free: true, Credits5s: 0,
		Resolutions: []string{"480p", "720p", "1080p"}, Durations: []string{"5", "10", "15", "20"},
		Note: "唯一有免费档的模型：480p/720p + tier=turbo + ≤10s；" +
			"官方通道 720p/5s=15、1080p/5s=20、720p/5s(base)=20 积分",
	},
	{
		ID: "seedance20", Channel: "both", WebReady: false, OfficialReady: true, Free: false, Credits5s: 68,
		Resolutions: []string{"480", "720", "1080"}, Durations: []string{"5", "10"},
		Note: "网页端 procedure 存在但**入参不同**（prompt/image/video/audio + 分辨率不带 p），" +
			"本服务未适配 ⇒ 请走 official；实测网页档 480p/5s 扣 68 积分，与官方同价",
	},
	{
		ID: "HappyHorse", Channel: "both", WebReady: false, OfficialReady: true, Free: false, Credits5s: 125,
		Resolutions: []string{"720P", "1080P"}, Durations: []string{"5"},
		Note: "网页 procedure 是小写 ai.happyhorse，但入参不同（prompt/duration/resolution）⇒ 请走 official；" +
			"官方 5s@720P = 125 积分",
	},
	{
		ID: "seedance25", Channel: "web", WebReady: true, Free: false, Credits5s: nil,
		Resolutions: []string{"480p", "720p"}, Durations: []string{"5"},
		Note: "网页端有 procedure（ai.seedance25），入参形态与 minimax 不同，本服务尚未适配；价格未实测",
	},
	{
		ID: "ltx23", Channel: "web", WebReady: false, Free: false, Credits5s: nil,
		Resolutions: []string{"480p", "720p"}, Durations: []string{"5"},
		Note: "网页端有 procedure（ai.ltx23）但入参完全不同（imageUrl/lastFrameUrl/middleFrame1-3Url + token），" +
			"本服务未适配；价格未实测",
	},
	{
		ID: "t2v", Channel: "official", OfficialReady: true, Free: false, Credits5s: 15,
		Durations: []string{"5", "8"},
		Note:      "官方独有；duraton 是字符串枚举",
	},
	{
		ID: "i2v", Channel: "official", OfficialReady: true, Free: false, Credits5s: 15,
		Durations: []string{"5", "8"},
		Note:      "官方独有；必须给首帧图（content 里带 image_url）",
	},
	{
		ID: "t2v_v3", Channel: "official", OfficialReady: true, Free: false, Credits5s: 20,
		Durations: []string{"5", "10", "15", "20"},
		Note:      "官方独有",
	},
	{
		ID: "i2v_v3", Channel: "official", OfficialReady: true, Free: false, Credits5s: 20,
		Durations: []string{"5", "10", "15", "20"},
		Note:      "官方独有；必须给首帧图",
	},
	{
		ID: "wan30", Channel: "official", OfficialReady: true, Free: false, Credits5s: 75,
		Resolutions: []string{"480P", "720P", "1080P"}, Durations: []string{"5", "10", "15"},
		Note: "官方独有；@480P 5s 为 40 积分",
	},
}

// modelEntryView 把内部结构翻成对外 JSON：OpenAI 那几个字段必须有，
// 我们的附加信息放 `aivideomaker` 下面，避免污染标准字段。
func modelEntryView(m modelInfo) map[string]any {
	out := map[string]any{
		"id":       m.ID,
		"object":   "model",
		"owned_by": "aivideomaker",
	}
	extra := map[string]any{
		"channel":        m.Channel,
		"web_ready":      m.WebReady,
		"official_ready": m.OfficialReady,
		"free":           m.Free,
		"note":           m.Note,
	}
	if m.Credits5s != nil {
		extra["credits_5s"] = m.Credits5s
	}
	if len(m.Resolutions) > 0 {
		extra["resolutions"] = m.Resolutions
	}
	if len(m.Durations) > 0 {
		extra["durations"] = m.Durations
	}
	out["aivideomaker"] = extra
	return out
}

// handleListModels 是 `GET /v1/models`：列出可用模型 + 走哪条通路 + 参考价。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	data := make([]map[string]any, 0, len(modelCatalog))
	for _, m := range modelCatalog {
		data = append(data, modelEntryView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleGetModel 是 `GET /v1/models/{id}`。
func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	for _, m := range modelCatalog {
		if strings.EqualFold(m.ID, id) {
			writeJSON(w, http.StatusOK, modelEntryView(m))
			return
		}
	}
	s.writeError(w, r, errUnknownModel(id))
}

// errUnknownModel 列出**可用**的模型，别让调用方猜。
func errUnknownModel(id string) error {
	ids := make([]string, 0, len(modelCatalog))
	for _, m := range modelCatalog {
		ids = append(ids, m.ID)
	}
	return apierr.Param("unknown model \""+id+"\"; available: "+strings.Join(ids, ", "), "model")
}

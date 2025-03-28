package glance

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	bilibiliWidgetTemplate             = mustParseTemplate("bilibili.html", "widget-base.html", "video-card-contents.html")
	bilibiliWidgetGridTemplate         = mustParseTemplate("bilibili-grid.html", "widget-base.html", "video-card-contents.html")
	bilibiliWidgetVerticalListTemplate = mustParseTemplate("bilibili-vertical-list.html", "widget-base.html")
	isDevelopment                      = os.Getenv("GLANCE_ENV") == "development" // 检查是否为开发环境
	// 根据开发模式设置日志处理器
	bilibiliLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: func() slog.Leveler {
			if isDevelopment {
				return slog.LevelDebug
			}
			return slog.LevelError
		}(),
	}))
)

// 日志包装函数
func blogInfo(msg string, args ...any) {
	if isDevelopment {
		bilibiliLogger.Info(msg, args...)
	}
}

func blogDebug(msg string, args ...any) {
	if isDevelopment {
		bilibiliLogger.Debug(msg, args...)
	}
}

func blogError(msg string, args ...any) {
	bilibiliLogger.Error(msg, args...)
}

func blogWarn(msg string, args ...any) {
	if isDevelopment {
		bilibiliLogger.Warn(msg, args...)
	}
}

// 创建一个带延迟的 HTTP 客户端
type delayedHTTPClient struct {
	client  *http.Client
	delay   time.Duration
	lastReq time.Time
}

func (c *delayedHTTPClient) Do(req *http.Request) (*http.Response, error) {
	blogDebug("执行HTTP请求",
		"URL", req.URL.String(),
		"Method", req.Method,
	)

	elapsed := time.Since(c.lastReq)
	if elapsed < c.delay {
		sleepTime := c.delay - elapsed
		blogDebug("请求延迟",
			"已经过时间", elapsed,
			"需要等待", sleepTime,
		)
		time.Sleep(sleepTime)
	}

	c.lastReq = time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		blogError("HTTP请求失败",
			"URL", req.URL.String(),
			"error", err,
		)
		return nil, err
	}

	blogDebug("HTTP请求完成",
		"URL", req.URL.String(),
		"状态码", resp.StatusCode,
	)

	return resp, err
}

var bilibiliHTTPClient = &delayedHTTPClient{
	client:  defaultHTTPClient,
	delay:   500 * time.Millisecond,
	lastReq: time.Time{},
}

type bilibiliUPConfig struct {
	UID   string        `yaml:"uid"`          // UP主ID
	Cache durationField `yaml:"update-every"` // 该UP主的自定义缓存时间
}

type bilibiliWidget struct {
	widgetBase        `yaml:",inline"`
	Videos            videoList           `yaml:"-"`
	Style             string              `yaml:"style"`
	CollapseAfter     int                 `yaml:"collapse-after"`
	CollapseAfterRows int                 `yaml:"collapse-after-rows"`
	UPs               []bilibiliUPConfig  `yaml:"ups"`          // UP主配置列表
	UpdateInterval    durationField       `yaml:"update-every"` // 默认更新间隔
	Limit             int                 `yaml:"limit"`
	cachedVideos      map[string]struct { // 每个UP主的视频缓存
		videos   []video
		expireAt time.Time
	}
	Error error
}

func (widget *bilibiliWidget) initialize() error {
	blogDebug("开始初始化哔哩哔哩模块")
	defer blogDebug("哔哩哔哩模块初始化完成")

	widget.withTitle("哔哩哔哩")
	widget.cachedVideos = make(map[string]struct {
		videos   []video
		expireAt time.Time
	})

	blogInfo("初始化哔哩哔哩模块",
		"开发模式", isDevelopment,
		"UP主数量", len(widget.UPs),
		"默认更新间隔", widget.UpdateInterval,
		"显示限制", widget.Limit,
	)

	if widget.Limit <= 0 {
		widget.Limit = 25
	}

	if widget.CollapseAfterRows == 0 || widget.CollapseAfterRows < -1 {
		widget.CollapseAfterRows = 4
	}

	if widget.CollapseAfter == 0 || widget.CollapseAfter < -1 {
		widget.CollapseAfter = 7
	}

	return nil
}

func (widget *bilibiliWidget) update(ctx context.Context) {
	blogDebug("开始执行哔哩哔哩模块更新",
		"context", fmt.Sprintf("%+v", ctx),
		"widget_config", fmt.Sprintf("%+v", widget),
	)
	defer blogDebug("哔哩哔哩模块更新执行完成")

	now := time.Now()
	allVideos := make(videoList, 0)
	var needUpdate []string

	// 在开发模式下，强制更新所有UP主的数据
	if isDevelopment {
		for _, up := range widget.UPs {
			needUpdate = append(needUpdate, up.UID)
		}
		blogInfo("开发模式：强制更新所有UP主数据", "UP主数量", len(needUpdate))
	} else {
		// 正常模式下检查缓存
		for _, up := range widget.UPs {
			cache, exists := widget.cachedVideos[up.UID]
			if !exists {
				blogDebug("UP主缓存不存在，需要更新", "UID", up.UID)
				needUpdate = append(needUpdate, up.UID)
			} else if now.After(cache.expireAt) {
				blogDebug("UP主缓存已过期，需要更新",
					"UID", up.UID,
					"过期时间", cache.expireAt,
					"缓存视频数", len(cache.videos),
				)
				needUpdate = append(needUpdate, up.UID)
			} else {
				blogDebug("使用UP主缓存数据",
					"UID", up.UID,
					"过期时间", cache.expireAt,
					"缓存视频数", len(cache.videos),
				)
				allVideos = append(allVideos, cache.videos...)
			}
		}
	}

	// 如果有需要更新的UP主
	if len(needUpdate) > 0 {
		blogInfo("开始更新UP主数据",
			"更新数量", len(needUpdate),
			"待更新UID列表", needUpdate,
		)

		newVideos, err := fetchBilibiliUserVideos(needUpdate)
		if err != nil {
			blogError("获取UP主视频失败",
				"error", err,
				"error_type", fmt.Sprintf("%T", err),
			)
			widget.Error = fmt.Errorf("获取视频失败: %w", err)
			return
		}

		if newVideos != nil {
			blogInfo("成功获取视频数据",
				"视频总数", len(newVideos),
				"更新UP主数", len(needUpdate),
			)
		}

		// 更新缓存
		for _, up := range widget.UPs {
			if !contains(needUpdate, up.UID) {
				continue
			}

			// 获取该UP主的缓存时间
			cacheDuration := 2 * time.Hour // 默认2小时
			if !isDevelopment {            // 非开发模式才应用缓存时间
				if time.Duration(widget.UpdateInterval) > 0 {
					cacheDuration = time.Duration(widget.UpdateInterval)
				}
				if time.Duration(up.Cache) > 0 {
					cacheDuration = time.Duration(up.Cache)
				}
			} else {
				// 开发模式下使用0秒缓存，即每次都刷新
				cacheDuration = 0
			}

			// 过滤出该UP主的视频
			upVideos := make([]video, 0)
			for _, v := range newVideos {
				if strings.HasSuffix(v.AuthorUrl, "/"+up.UID) {
					upVideos = append(upVideos, v)
				}
			}

			blogDebug("更新UP主缓存",
				"UID", up.UID,
				"视频数", len(upVideos),
				"缓存时间", cacheDuration,
			)

			// 更新缓存
			widget.cachedVideos[up.UID] = struct {
				videos   []video
				expireAt time.Time
			}{
				videos:   upVideos,
				expireAt: now.Add(cacheDuration),
			}

			allVideos = append(allVideos, upVideos...)
		}
	} else {
		blogDebug("所有UP主数据均在缓存中，无需更新")
	}

	// 排序并限制数量
	allVideos.sortByNewest()
	if len(allVideos) > widget.Limit {
		blogDebug("截断视频列表到限制数量",
			"原数量", len(allVideos),
			"限制数量", widget.Limit,
		)
		allVideos = allVideos[:widget.Limit]
	}

	widget.Videos = allVideos
	blogInfo("哔哩哔哩模块更新完成",
		"最终视频数", len(allVideos),
		"UP主总数", len(widget.UPs),
	)
}

// 辅助函数：检查字符串是否在切片中
func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func (widget *bilibiliWidget) Render() template.HTML {
	blogDebug("开始渲染哔哩哔哩模块",
		"style", widget.Style,
		"视频数量", len(widget.Videos),
	)
	defer blogDebug("哔哩哔哩模块渲染完成")

	// 如果没有视频数据，强制执行一次更新
	if len(widget.Videos) == 0 {
		blogInfo("检测到没有视频数据，执行强制更新")
		widget.update(context.Background())

		// 更新后再次检查视频数量
		if len(widget.Videos) == 0 {
			blogError("更新后仍然没有视频数据")
			widget.Error = fmt.Errorf("无法获取视频数据，请检查网络连接或UP主ID是否正确")
		}
	}

	var template *template.Template
	switch widget.Style {
	case "grid-cards":
		template = bilibiliWidgetGridTemplate
	case "vertical-list":
		template = bilibiliWidgetVerticalListTemplate
	default:
		template = bilibiliWidgetTemplate
	}

	// 在渲染前检查是否有错误
	if widget.Error != nil {
		blogError("渲染时发现错误", "error", widget.Error)
	}

	return widget.renderTemplate(widget, template)
}

type bilibiliVideoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List struct {
			Vlist []struct {
				Title   string `json:"title"`
				Author  string `json:"author"`
				Aid     int    `json:"aid"`
				Bvid    string `json:"bvid"`
				Pic     string `json:"pic"`
				Created int64  `json:"created"`
			} `json:"vlist"`
		} `json:"list"`
	} `json:"data"`
}

func fetchBilibiliUserVideos(uids []string) (videoList, error) {
	blogDebug("准备发起B站API请求", "UP主列表", uids)

	requests := make([]*http.Request, 0, len(uids))
	for _, uid := range uids {
		apiUrl := fmt.Sprintf("https://api.bilibili.com/x/space/arc/search?mid=%s&ps=30&tid=0&pn=1&order=pubdate", uid)
		blogDebug("构建请求",
			"UID", uid,
			"URL", apiUrl,
		)
		request, err := http.NewRequest("GET", apiUrl, nil)
		if err != nil {
			blogError("创建请求失败",
				"UID", uid,
				"error", err,
			)
			continue
		}

		// 添加更多必要的请求头
		request.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
		request.Header.Add("Accept", "application/json, text/plain, */*")
		request.Header.Add("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		request.Header.Add("Origin", "https://space.bilibili.com")
		request.Header.Add("Referer", fmt.Sprintf("https://space.bilibili.com/%s/video", uid))

		blogDebug("请求头设置完成",
			"UID", uid,
			"headers", request.Header,
		)

		requests = append(requests, request)
	}

	blogInfo("开始执行并发请求",
		"请求数量", len(requests),
		"并发数", 2,
	)

	// 使用带延迟的客户端，并减少并发数
	job := newJob(decodeJsonFromRequestTask[bilibiliVideoResponse](bilibiliHTTPClient), requests).
		withWorkers(2) // 减少并发数到2

	responses, errs, err := workerPoolDo(job)
	if err != nil {
		blogError("请求处理失败", "error", err)
		return nil, fmt.Errorf("%w: %v", errNoContent, err)
	}

	videos := make(videoList, 0, len(uids)*30)
	var failed int

	for i := range responses {
		if errs[i] != nil {
			blogError("请求执行失败",
				"UID", uids[i],
				"error", errs[i],
				"error_type", fmt.Sprintf("%T", errs[i]),
			)
			failed++
			continue
		}

		response := responses[i]
		blogDebug("收到B站响应",
			"UID", uids[i],
			"状态码", response.Code,
			"消息", response.Message,
			"数据大小", len(response.Data.List.Vlist),
		)

		if response.Code != 0 {
			failed++
			blogError("B站API返回错误",
				"UID", uids[i],
				"错误码", response.Code,
				"错误信息", response.Message,
			)
			continue
		}

		videoCount := len(response.Data.List.Vlist)
		blogDebug("成功获取UP主视频",
			"UID", uids[i],
			"视频数量", videoCount,
		)

		for _, v := range response.Data.List.Vlist {
			videos = append(videos, video{
				ThumbnailUrl: v.Pic,
				Title:        v.Title,
				Url:          fmt.Sprintf("https://www.bilibili.com/video/%s", v.Bvid),
				Author:       v.Author,
				AuthorUrl:    fmt.Sprintf("https://space.bilibili.com/%s", uids[i]),
				TimePosted:   time.Unix(v.Created, 0),
			})
		}
	}

	if len(videos) == 0 {
		blogError("未获取到任何视频")
		return nil, errNoContent
	}

	videos.sortByNewest()

	if failed > 0 {
		blogWarn("部分UP主数据获取失败",
			"失败数量", failed,
			"总UP主数", len(uids),
		)
		return videos, fmt.Errorf("%w: missing videos from %d users", errPartialContent, failed)
	}

	blogInfo("全部UP主视频获取完成",
		"视频总数", len(videos),
		"UP主数量", len(uids),
	)
	return videos, nil
}

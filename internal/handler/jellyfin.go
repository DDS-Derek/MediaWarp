package handler

import (
	"MediaWarp/constants"
	"MediaWarp/internal/config"
	"MediaWarp/internal/logging"
	"MediaWarp/internal/service"
	"MediaWarp/internal/service/jellyfin"
	"MediaWarp/utils"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Jellyfin 服务器处理器
type JellyfinHandler struct {
	server      *jellyfin.Jellyfin     // Jellyfin 服务器
	routerRules []RegexpRouteRule      // 正则路由规则
	proxy       *httputil.ReverseProxy // 反向代理
}

func NewJellyfinHander(addr string, apiKey string) (*JellyfinHandler, error) {
	jellyfinHandler := JellyfinHandler{}
	jellyfinHandler.server = jellyfin.New(addr, apiKey)
	target, err := url.Parse(jellyfinHandler.server.GetEndpoint())
	if err != nil {
		return nil, err
	}
	jellyfinHandler.proxy = httputil.NewSingleHostReverseProxy(target)

	{ // 初始化路由规则
		jellyfinHandler.routerRules = []RegexpRouteRule{
			{
				Regexp: constants.JellyfinRegexp.Router.ModifyPlaybackInfo,
				Handler: responseModifyCreater(
					&httputil.ReverseProxy{Director: jellyfinHandler.proxy.Director},
					jellyfinHandler.ModifyPlaybackInfo,
				),
			},
			{
				Regexp:  constants.JellyfinRegexp.Router.VideosHandler,
				Handler: jellyfinHandler.VideosHandler,
			},
		}
		if config.Web.Enable {
			if config.Web.Index || config.Web.Head != "" || config.Web.ExternalPlayerUrl || config.Web.VideoTogether {
				jellyfinHandler.routerRules = append(
					jellyfinHandler.routerRules,
					RegexpRouteRule{
						Regexp: constants.JellyfinRegexp.Router.ModifyIndex,
						Handler: responseModifyCreater(
							&httputil.ReverseProxy{Director: jellyfinHandler.proxy.Director},
							jellyfinHandler.ModifyIndex,
						),
					},
				)
			}
		}
	}
	return &jellyfinHandler, nil
}

// 转发请求至上游服务器
func (jellyfinHandler *JellyfinHandler) ReverseProxy(rw http.ResponseWriter, req *http.Request) {
	jellyfinHandler.proxy.ServeHTTP(rw, req)
}

// 正则路由表
func (jellyfinHandler *JellyfinHandler) GetRegexpRouteRules() []RegexpRouteRule {
	return jellyfinHandler.routerRules
}

// 修改播放信息请求
//
// /Items/:itemId
// 强制将 HTTPStrm 设置为支持直链播放和转码、AlistStrm 设置为支持直链播放并且禁止转码
func (jellyfinHandler *JellyfinHandler) ModifyPlaybackInfo(rw *http.Response) error {
	defer rw.Body.Close()
	data, err := readBody(rw)
	if err != nil {
		logging.Warning("读取响应体失败：", err)
		return err
	}

	var playbackInfoResponse jellyfin.PlaybackInfoResponse
	if err = json.Unmarshal(data, &playbackInfoResponse); err != nil {
		logging.Warning("解析 jellyfin.PlaybackInfoResponse JSON 错误：", err)
		return err
	}

	for index, mediasource := range playbackInfoResponse.MediaSources {
		logging.Debug("请求 ItemsServiceQueryItem：" + *mediasource.ID)
		itemResponse, err := jellyfinHandler.server.ItemsServiceQueryItem(*mediasource.ID, 1, "Path,MediaSources") // 查询 item 需要去除前缀仅保留数字部分
		if err != nil {
			logging.Warning("请求 ItemsServiceQueryItem 失败：", err)
			continue
		}
		item := itemResponse.Items[0]
		strmFileType, opt := recgonizeStrmFileType(*item.Path)
		switch strmFileType {
		case constants.HTTPStrm: // HTTPStrm 设置支持直链播放并且支持转码
			if !config.HTTPStrm.TransCode {
				*playbackInfoResponse.MediaSources[index].SupportsDirectPlay = true
				*playbackInfoResponse.MediaSources[index].SupportsDirectStream = true
				playbackInfoResponse.MediaSources[index].TranscodingURL = nil
				playbackInfoResponse.MediaSources[index].TranscodingSubProtocol = nil
				playbackInfoResponse.MediaSources[index].TranscodingContainer = nil
				if mediasource.DirectStreamURL != nil {
					apikeypair, err := utils.ResolveEmbyAPIKVPairs(*mediasource.DirectStreamURL)
					if err != nil {
						logging.Warning("解析API键值对失败：", err)
						continue
					}
					directStreamURL := fmt.Sprintf("/Videos/%s/stream?MediaSourceId=%s&Static=true&%s", *mediasource.ID, *mediasource.ID, apikeypair)
					playbackInfoResponse.MediaSources[index].DirectStreamURL = &directStreamURL
					logging.Info(*mediasource.Name, " 强制禁止转码，直链播放链接为: ", directStreamURL)
				}
			}

		case constants.AlistStrm: // AlistStm 设置支持直链播放并且禁止转码
			if !config.AlistStrm.TransCode {
				*playbackInfoResponse.MediaSources[index].SupportsDirectPlay = true
				*playbackInfoResponse.MediaSources[index].SupportsDirectStream = true
				*playbackInfoResponse.MediaSources[index].SupportsTranscoding = false
				playbackInfoResponse.MediaSources[index].TranscodingURL = nil
				playbackInfoResponse.MediaSources[index].TranscodingSubProtocol = nil
				playbackInfoResponse.MediaSources[index].TranscodingContainer = nil
				directStreamURL := fmt.Sprintf("/Videos/%s/stream?MediaSourceId=%s&Static=true", *mediasource.ID, *mediasource.ID)
				if mediasource.DirectStreamURL != nil {
					logging.Debugf("%s 原直链播放链接: %s", *mediasource.Name, *mediasource.DirectStreamURL)
					apikeypair, err := utils.ResolveEmbyAPIKVPairs(*mediasource.DirectStreamURL)
					if err != nil {
						logging.Warning("解析API键值对失败：", err)
						continue
					}
					directStreamURL += "&" + apikeypair
				}
				playbackInfoResponse.MediaSources[index].DirectStreamURL = &directStreamURL
				container := strings.TrimPrefix(path.Ext(*mediasource.Path), ".")
				playbackInfoResponse.MediaSources[index].Container = &container
				logging.Info(*mediasource.Name, " 强制禁止转码，直链播放链接为: ", directStreamURL, "，容器为: ", container)
			} else {
				logging.Info(*mediasource.Name, " 保持原有转码设置")
			}

			if playbackInfoResponse.MediaSources[index].Size == nil {
				alistServer, err := service.GetAlistServer(opt.(string))
				if err != nil {
					logging.Warning("获取 AlistServer 失败：", err)
					continue
				}
				fsGetData, err := alistServer.FsGet(*mediasource.Path)
				if err != nil {
					logging.Warning("请求 FsGet 失败：", err)
					continue
				}
				playbackInfoResponse.MediaSources[index].Size = &fsGetData.Size
				logging.Info(*mediasource.Name, "设置文件大小为:", fsGetData.Size)
			}
		}
	}

	if data, err = json.Marshal(playbackInfoResponse); err != nil {
		logging.Warning("序列化 jellyfin.PlaybackInfoResponse Json 错误：", err)
		return err
	}

	rw.Header.Set("Content-Type", "application/json") // 更新 Content-Type 头
	return updateBody(rw, data)
}

// 视频流处理器
//
// 支持播放本地视频、重定向 HttpStrm、AlistStrm
func (jellyfinHandler *JellyfinHandler) VideosHandler(ctx *gin.Context) {
	if ctx.Request.Method == http.MethodHead { // 不额外处理 HEAD 请求
		jellyfinHandler.ReverseProxy(ctx.Writer, ctx.Request)
		logging.Debug("VideosHandler 不处理 HEAD 请求，转发至上游服务器")
		return
	}

	mediaSourceID := ctx.Query("mediasourceid")
	logging.Debug("请求 ItemsServiceQueryItem：", mediaSourceID)
	itemResponse, err := jellyfinHandler.server.ItemsServiceQueryItem(mediaSourceID, 1, "Path,MediaSources") // 查询 item 需要去除前缀仅保留数字部分
	if err != nil {
		logging.Warning("请求 ItemsServiceQueryItem 失败：", err)
		jellyfinHandler.proxy.ServeHTTP(ctx.Writer, ctx.Request)
		return
	}

	item := itemResponse.Items[0]

	if !strings.HasSuffix(strings.ToLower(*item.Path), ".strm") { // 不是 Strm 文件
		logging.Debug("播放本地视频：" + *item.Path + "，不进行处理")
		jellyfinHandler.proxy.ServeHTTP(ctx.Writer, ctx.Request)
		return
	}

	strmFileType, opt := recgonizeStrmFileType(*item.Path)
	for _, mediasource := range item.MediaSources {
		if *mediasource.ID == mediaSourceID { // EmbyServer >= 4.9 返回的ID带有前缀mediasource_
			switch strmFileType {
			case constants.HTTPStrm:
				if *mediasource.Protocol == jellyfin.HTTP {
					// 创建不自动跳转的HTTP Client
					noRedirectClient := &http.Client{
						Timeout: 10 * time.Second, // 增加超时设置
						CheckRedirect: func(req *http.Request, via []*http.Request) error {
							return http.ErrUseLastResponse
						},
					}

					// 对原始URL进行解析和编码
					baseURL, err := url.Parse(*mediasource.Path)
					if err != nil {
						logging.Warning("HTTPStrm[0] 解析原始URL失败:", err)
						return
					}

					// 对查询参数进行编码
					query := baseURL.Query()
					encodedQuery := url.Values{}
					for key, values := range query {
						for _, value := range values {
							encodedQuery.Add(key, value)
						}
					}
					baseURL.RawQuery = encodedQuery.Encode()

					firstReq, err := http.NewRequest("HEAD", baseURL.String(), nil)
					if err != nil {
						logging.Warning("HTTPStrm[1] 请求创建失败:", err)
						return
					}
					if ua := ctx.Request.UserAgent(); ua != "" {
						firstReq.Header.Set("User-Agent", ua)
					} else {
						firstReq.Header.Set("User-Agent", "")
					}

					firstResp, err := noRedirectClient.Do(firstReq)
					if err != nil {
						logging.Warning("HTTPStrm[1] 请求失败:", err)
						return
					}
					defer firstResp.Body.Close()

					// 检查是否是302且Location是/302/?pickcode=格式
					if firstResp.StatusCode == http.StatusFound || firstResp.StatusCode == http.StatusMovedPermanently {
						location := firstResp.Header.Get("Location")
						if location == "" {
							logging.Warning("HTTPStrm[1] 请求返回302但没有Location头")
							return
						}

						var finalURL string

						if strings.HasPrefix(location, "/302/?pickcode=") {
							fullURL := fmt.Sprintf("%s://%s%s", baseURL.Scheme, baseURL.Host, location)
							logging.Info("HTTPStrm[2] 拼接完整URL:", fullURL)

							// 对跳转URL也进行编码处理
							redirectURL, err := url.Parse(fullURL)
							if err != nil {
								logging.Warning("HTTPStrm[2] 解析跳转URL失败:", err)
								return
							}

							// 编码跳转URL的查询参数
							redirectQuery := redirectURL.Query()
							encodedRedirectQuery := url.Values{}
							for key, values := range redirectQuery {
								for _, value := range values {
									encodedRedirectQuery.Add(key, value)
								}
							}
							redirectURL.RawQuery = encodedRedirectQuery.Encode()

							secondReq, err := http.NewRequest("HEAD", redirectURL.String(), nil)
							if err != nil {
								logging.Warning("HTTPStrm[2] 请求创建失败:", err)
								return
							}
							secondReq.Header = firstReq.Header.Clone()

							secondResp, err := noRedirectClient.Do(secondReq)
							if err != nil {
								logging.Warning("HTTPStrm[2] 请求失败:", err)
								return
							}
							defer secondResp.Body.Close()

							if secondResp.StatusCode == http.StatusFound || secondResp.StatusCode == http.StatusMovedPermanently {
								finalLocation := secondResp.Header.Get("Location")
								if finalLocation == "" {
									logging.Warning("HTTPStrm[2] 请求返回302但没有Location头")
									return
								}
								finalURL = finalLocation
							} else {
								finalURL = redirectURL.String()
							}
						} else {
							finalURL = location
						}

						logging.Info("HTTPStrm 获取到最终跳转地址:", finalURL)
						ctx.Redirect(http.StatusFound, finalURL)
						return
					}

					// 如果不是302，直接返回编码后的原始URL
					ctx.Redirect(http.StatusFound, baseURL.String())
				}
				return
			case constants.AlistStrm: // 无需判断 *mediasource.Container 是否以Strm结尾，当 AlistStrm 存储的位置有对应的文件时，*mediasource.Container 会被设置为文件后缀
				alistServerAddr := opt.(string)
				alistServer, err := service.GetAlistServer(alistServerAddr)
				if err != nil {
					logging.Warning("获取 AlistServer 失败：", err)
					return
				}
				fsGetData, err := alistServer.FsGet(*mediasource.Path)
				if err != nil {
					logging.Warning("请求 FsGet 失败：", err)
					return
				}
				var redirectURL string
				if config.AlistStrm.RawURL {
					redirectURL = fsGetData.RawURL
				} else {
					redirectURL = fmt.Sprintf("%s/d%s", alistServerAddr, *mediasource.Path)
					if fsGetData.Sign != "" {
						redirectURL += "?sign=" + fsGetData.Sign
					}
				}
				logging.Info("AlistStrm 重定向至：", redirectURL)
				ctx.Redirect(http.StatusFound, redirectURL)
				return
			case constants.UnknownStrm:
				jellyfinHandler.proxy.ServeHTTP(ctx.Writer, ctx.Request)
				return
			}
		}
	}
}

// 修改首页函数
func (jellyfinHandler *JellyfinHandler) ModifyIndex(rw *http.Response) error {
	var (
		htmlFilePath string = path.Join(config.CostomDir(), "index.html")
		htmlContent  []byte
		addHEAD      []byte
		err          error
	)

	defer rw.Body.Close() // 无论哪种情况，最终都要确保原 Body 被关闭，避免内存泄漏
	if config.Web.Index { // 从本地文件读取index.html
		if htmlContent, err = os.ReadFile(htmlFilePath); err != nil {
			logging.Warning("读取文件内容出错，错误信息：", err)
			return err
		}
	} else { // 从上游获取响应体
		if htmlContent, err = readBody(rw); err != nil {
			return err
		}
	}

	if config.Web.Head != "" { // 用户自定义HEAD
		addHEAD = append(addHEAD, []byte(config.Web.Head+"\n")...)
	}
	if config.Web.ExternalPlayerUrl { // 外部播放器
		addHEAD = append(addHEAD, []byte(`<script src="/MediaWarp/static/embyExternalUrl/embyWebAddExternalUrl/embyLaunchPotplayer.js"></script>`+"\n")...)
	}
	if config.Web.Crx { // crx 美化
		addHEAD = append(addHEAD, []byte(`<link rel="stylesheet" id="theme-css" href="/MediaWarp/static/jellyfin-crx/static/css/style.css" type="text/css" media="all" />
    <script src="/MediaWarp/static/jellyfin-crx/static/js/common-utils.js"></script>
    <script src="/MediaWarp/static/jellyfin-crx/static/js/jquery-3.6.0.min.js"></script>
    <script src="/MediaWarp/static/jellyfin-crx/static/js/md5.min.js"></script>
    <script src="/MediaWarp/static/jellyfin-crx/content/main.js"></script>`+"\n")...)
	}
	if config.Web.ActorPlus { // 过滤没有头像的演员和制作人员
		addHEAD = append(addHEAD, []byte(`<script src="/MediaWarp/static/emby-web-mod/actorPlus/actorPlus.js"></script>`+"\n")...)
	}
	if config.Web.FanartShow { // 显示同人图（fanart图）
		addHEAD = append(addHEAD, []byte(`<script src="/MediaWarp/static/emby-web-mod/fanart_show/fanart_show.js"></script>`+"\n")...)
	}
	if config.Web.Danmaku { // 弹幕
		addHEAD = append(addHEAD, []byte(`<script src="/MediaWarp/static/jellyfin-danmaku/ede.js" defer></script>`+"\n")...)
	}
	if config.Web.VideoTogether { // VideoTogether
		addHEAD = append(addHEAD, []byte(`<script src="https://2gether.video/release/extension.website.user.js"></script>`+"\n")...)
	}
	htmlContent = bytes.Replace(htmlContent, []byte("</head>"), append(addHEAD, []byte("</head>")...), 1) // 将添加HEAD

	return updateBody(rw, htmlContent)
}

var _ MediaServerHandler = (*JellyfinHandler)(nil) // 确保 JellyfinHandler 实现 MediaServerHandler 接口

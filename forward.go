package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amarnathcjd/gogram/telegram"
)

// 转发功能: /forward <t.me 消息链接> [目标]
//
// 1. 先尝试原生转发(保留来源, 不占带宽);
// 2. 目标频道开了"禁止转发/保存"时, 原生转发会被拒绝, 改为 UserBot 下载后重新上传,
//    保留正文、格式实体以及视频/音频/文件属性(时长、分辨率、文件名)。
// 目标省略时发到 UserBot 账号自己的「收藏夹」(Saved Messages)。
//
// 安全: 转发是以 UserBot 账号(真人号)的身份发消息, 所以只允许管理员使用,
// 白名单用户不可用, 防止别人借你的号给任意会话发东西。

const (
	forwardTimeout = 60 * time.Minute // 单条链接(含相册)的最长处理时间, 大视频下载+上传需要较久
	forwardTmpDir  = "forward_tmp"    // 下载中转目录, 位于 files 目录下, 任务结束即删除
)

// forwardMu 保证同一时间只跑一个转发任务, 避免并发大文件把磁盘/带宽打满并触发风控
var forwardMu sync.Mutex

// handleForward 处理 /forward 命令
func handleForward(m *telegram.NewMessage) error {
	if !infos.needAdmin(m) {
		return nil
	}

	args := strings.Fields(m.Args())
	if len(args) < 1 || len(args) > 2 {
		sendMS(m, "用法: /forward <消息链接> [目标]\n"+
			"目标可省略(发到收藏夹), 或填 @用户名 / 数字ID / me / t.me 频道链接\n"+
			"例: /forward https://t.me/c/1234567890/567\n"+
			"例: /forward https://t.me/somechannel/89 @mychannel", nil, 120)
		return nil
	}

	client := infos.UserClient.Load()
	if client == nil || infos.Status.Load() != 3 {
		sendMS(m, "UserBot 未登录, 无法转发, 请先 /qr 或 /phone 登录", nil, 60)
		return nil
	}

	matches := teleLinkRe.FindAllStringSubmatch(args[0], -1)
	if len(matches) == 0 {
		sendMS(m, "未识别到 t.me 消息链接", nil, 60)
		return nil
	}

	targetArg := "me"
	if len(args) == 2 {
		targetArg = args[1]
	}
	target, err := forwardTarget(targetArg)
	if err != nil {
		sendMS(m, fmt.Sprintf("转发目标无效: %v", err), nil, 60)
		return nil
	}
	if _, err := client.ResolvePeer(target); err != nil {
		sendMS(m, fmt.Sprintf("无法访问转发目标 %s: %v\n(UserBot 账号需要能在该会话发言)", targetArg, err), nil, 60)
		return nil
	}

	if !forwardMu.TryLock() {
		sendMS(m, "已有转发任务在进行, 请等它完成后再试", nil, 60)
		return nil
	}

	sendMS(m, "开始转发, 大文件需要一些时间...", nil, 60)
	safeGo(func() {
		defer forwardMu.Unlock()
		var done, failed int
		for _, match := range matches {
			mode, err := forwardOne(client, target, match)
			if err != nil {
				failed++
				log.Printf("转发失败 %s: %+v", match[0], err)
				sendMS(m, fmt.Sprintf("转发失败 %s: %v", match[0], err), nil, 300)
				continue
			}
			done++
			log.Printf("转发成功 %s -> %v (%s)", match[0], target, mode)
		}
		sendMS(m, fmt.Sprintf("转发结束: 成功 %d 条, 失败 %d 条", done, failed), nil, 300)
	})
	return nil
}

// forwardTarget 把目标参数规整为 ResolvePeer 认得的形态
func forwardTarget(arg string) (any, error) {
	arg = strings.TrimSpace(arg)
	switch {
	case arg == "":
		return nil, errors.New("目标为空")
	case strings.EqualFold(arg, "me") || arg == "收藏夹":
		return "me", nil
	}
	// t.me/c/123 或 t.me/name 形式的频道链接(可带或不带消息 ID)
	if strings.Contains(arg, "t.me/") {
		rest := arg[strings.Index(arg, "t.me/")+len("t.me/"):]
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) >= 2 && parts[0] == "c" && isAllNumber(parts[1]) {
			var id int64
			if _, err := fmt.Sscan("-100"+parts[1], &id); err != nil {
				return nil, err
			}
			return id, nil
		}
		if len(parts) >= 1 && parts[0] != "" && parts[0] != "c" {
			return parts[0], nil
		}
		return nil, fmt.Errorf("无法解析链接 %s", arg)
	}
	value := strings.TrimPrefix(arg, "@")
	if isAllNumber(strings.TrimPrefix(value, "-")) {
		var id int64
		if _, err := fmt.Sscan(value, &id); err != nil {
			return nil, err
		}
		return id, nil
	}
	return value, nil
}

// fwdSourcePeer 把链接 submatch 解析为来源会话和消息 ID
func fwdSourcePeer(match []string) (peer any, mid int32, err error) {
	if _, err = fmt.Sscan(match[4], &mid); err != nil {
		return nil, 0, fmt.Errorf("解析消息ID失败: %w", err)
	}
	if match[2] != "" {
		var cid int64
		if _, err = fmt.Sscan("-100"+match[2], &cid); err != nil {
			return nil, 0, fmt.Errorf("解析频道ID失败: %w", err)
		}
		return cid, mid, nil
	}
	return match[3], mid, nil
}

// forwardOne 处理单条链接, 返回实际采用的方式
func forwardOne(client *telegram.Client, target any, match []string) (string, error) {
	source, mid, err := fwdSourcePeer(match)
	if err != nil {
		return "", err
	}
	return forwardByID(client, target, source, mid)
}

// forwardByID 转发来源会话中的一条消息(相册会整组转发), 返回实际采用的方式
func forwardByID(client *telegram.Client, target, source any, mid int32) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()

	ms, err := client.GetMessages(source, &telegram.SearchOption{IDs: []int32{mid}, Context: ctx})
	if err != nil {
		return "", fmt.Errorf("获取消息失败: %w", err)
	}
	if len(ms) == 0 || ms[0].Message == nil {
		return "", fmt.Errorf("没有找到这条消息(mid=%d), 确认 UserBot 账号已加入该频道/群组", mid)
	}

	group := []telegram.NewMessage{ms[0]}
	if ms[0].Message.GroupedID != 0 {
		if values, err := ms[0].GetMediaGroup(); err == nil && len(values) > 0 {
			group = values
		}
	}

	// 1. 原生转发, 能转就不下载
	ids := make([]int32, 0, len(group))
	for _, item := range group {
		ids = append(ids, item.ID)
	}
	if _, err := client.Forward(target, source, ids); err == nil {
		return "原生转发", nil
	} else if !isForwardRestricted(err) {
		log.Printf("原生转发失败, 改用下载重传: %+v", err)
	}

	// 2. 下载后重新上传
	dir, err := os.MkdirTemp(filepath.Join(infos.FilesPath), forwardTmpDir+"-")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(dir)

	if len(group) == 1 {
		return "下载重传", copyMessage(ctx, client, target, group[0], dir)
	}
	return "下载重传(相册)", copyAlbum(ctx, client, target, group, dir)
}

// isForwardRestricted 判断是否为"禁止转发"类错误
func isForwardRestricted(err error) bool {
	text := err.Error()
	return strings.Contains(text, "CHAT_FORWARDS_RESTRICTED") || strings.Contains(text, "FORWARDS_RESTRICTED")
}

// copyMessage 复制单条消息(文本或媒体)
func copyMessage(ctx context.Context, client *telegram.Client, target any, msg telegram.NewMessage, dir string) error {
	text := msg.Message.Message
	entities := msg.Message.Entities

	if !msg.IsMedia() || !downloadable(msg) {
		if text == "" {
			return errors.New("消息既无正文也无可下载的媒体")
		}
		_, err := client.SendMessage(target, text, &telegram.SendOptions{Entities: entities, ParseMode: "none"})
		return err
	}

	path, err := downloadTo(ctx, client, msg, dir, 0)
	if err != nil {
		return err
	}
	opts := mediaOptions(msg)
	opts.Caption = text
	opts.Entities = entities
	if _, err := client.SendMedia(target, path, opts); err != nil {
		return fmt.Errorf("上传失败: %w", err)
	}
	return nil
}

// copyAlbum 复制相册, 每一项保留自己的说明文字
func copyAlbum(ctx context.Context, client *telegram.Client, target any, group []telegram.NewMessage, dir string) error {
	var paths []string
	var captions []string
	for i, msg := range group {
		if !msg.IsMedia() || !downloadable(msg) {
			continue
		}
		path, err := downloadTo(ctx, client, msg, dir, i)
		if err != nil {
			return err
		}
		paths = append(paths, path)
		captions = append(captions, msg.Message.Message)
	}
	switch len(paths) {
	case 0:
		return errors.New("相册里没有可下载的媒体")
	case 1:
		return copyMessage(ctx, client, target, group[0], dir)
	}
	if _, err := client.SendAlbum(target, paths, &telegram.MediaOptions{Caption: captions, ParseMode: "none"}); err != nil {
		return fmt.Errorf("上传相册失败: %w", err)
	}
	return nil
}

// downloadable 只处理照片和文件类媒体(视频/音频/文档/GIF); 投票、位置、网页预览等无法下载
func downloadable(msg telegram.NewMessage) bool {
	return msg.Photo() != nil || msg.Document() != nil
}

// downloadTo 把媒体下载到临时目录, 返回本地路径
func downloadTo(ctx context.Context, client *telegram.Client, msg telegram.NewMessage, dir string, index int) (string, error) {
	name := fmt.Sprintf("%02d_%s", index, safeFileName(mediaName(msg)))
	path := filepath.Join(dir, name)
	if _, err := client.DownloadMedia(msg.Media(), &telegram.DownloadOptions{
		FileName: path,
		Ctx:      ctx,
		Threads:  max(1, int(infos.Conf.Load().Workers)),
	}); err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	return path, nil
}

// mediaOptions 复制原文件属性, 让视频仍以视频(带时长/分辨率/可串流)显示, 文件保留原名
func mediaOptions(msg telegram.NewMessage) *telegram.MediaOptions {
	opts := &telegram.MediaOptions{ParseMode: "none"}
	if doc := msg.Document(); doc != nil {
		opts.MimeType = doc.MimeType
		opts.Attributes = doc.Attributes
		opts.FileName = mediaName(msg)
	}
	return opts
}

// mediaName 尽量还原原始文件名
func mediaName(msg telegram.NewMessage) string {
	if doc := msg.Document(); doc != nil {
		for _, attr := range doc.Attributes {
			if value, ok := attr.(*telegram.DocumentAttributeFilename); ok && value.FileName != "" {
				return value.FileName
			}
		}
		return fmt.Sprintf("file_%d%s", msg.ID, mimeExt(doc.MimeType))
	}
	if msg.Photo() != nil {
		return fmt.Sprintf("photo_%d.jpg", msg.ID)
	}
	return fmt.Sprintf("file_%d.bin", msg.ID)
}

// safeFileName 去掉路径分隔符等不安全字符
func safeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "file.bin"
	}
	return name
}

var mimeExts = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
	"video/mp4": ".mp4", "video/webm": ".webm", "video/quicktime": ".mov", "video/x-matroska": ".mkv",
	"audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/mp4": ".m4a", "audio/flac": ".flac",
	"application/pdf": ".pdf", "application/zip": ".zip", "application/x-tgsticker": ".tgs",
}

func mimeExt(mime string) string {
	if ext, ok := mimeExts[strings.ToLower(strings.TrimSpace(mime))]; ok {
		return ext
	}
	return ".bin"
}

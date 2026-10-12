package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/amarnathcjd/gogram/telegram"
)

// 按钮式转发, 不用复制链接:
//
//	/fw [关键词]   选来源会话 -> 勾选消息 -> 选目标会话, 一次性转发
//	/to 关键词     在「选目标」这一步按名字过滤会话列表
//	/autofw        自动转发规则: 来源会话有新消息就转到目标(频道/群/人/机器人都行)
//
// 会话列表来自 UserBot 账号自己的聊天列表, 所以频道、群、私聊、机器人都能选。
// 思路参考 PetroVoronov/telegram-forward-user-bot(按钮菜单配规则)、
// viperadnan-git/telegram-forwarder-bot(从聊天列表里点选, 不用填 ID)、
// HanuTyagi/TeleCopy(挑消息复制 + 实时监听转发)。

const (
	fwPageSize   = 8
	fwMsgPage    = 10
	fwDialogMax  = 200
	fwSessionTTL = 30 * time.Minute
	fwRulesFile  = "forward_rules.json"
)

type fwDialog struct {
	ID   int64  `json:"id"`   // 带标记的 ID: 频道 -100xxx, 普通群 -xxx, 用户/机器人为正数
	Name string `json:"name"` // 显示名
	Kind string `json:"kind"` // 图标
}

type fwMsg struct {
	ID      int32
	Grouped int64
	Label   string
}

type fwSession struct {
	Mode     string // once: 一次性转发; auto: 添加自动规则
	Step     string // src / msg / dst
	Dialogs  []fwDialog
	Page     int
	Source   fwDialog
	Msgs     []fwMsg
	Selected map[int32]bool
	Updated  time.Time
}

type fwRule struct {
	Source fwDialog `json:"source"`
	Target fwDialog `json:"target"`
}

var (
	fwSessions   = map[int64]*fwSession{}
	fwSessionsMu sync.Mutex
	fwRules      []fwRule
	fwRulesMu    sync.RWMutex
	fwAlbums     = map[int64]*fwAlbumBuf{}
	fwAlbumsMu   sync.Mutex
	fwAutoMu     sync.Mutex
	fwRulesOnce  sync.Once
)

// ---------- 命令入口 ----------

// handleFwPicker 处理 /fw [关键词] 与 /autofw
func handleFwPicker(m *telegram.NewMessage, mode string) error {
	if !infos.needAdmin(m) {
		return nil
	}
	client := infos.UserClient.Load()
	if client == nil || infos.Status.Load() != 3 {
		sendMS(m, "UserBot 未登录, 请先 /qr 或 /phone 登录", nil, 60)
		return nil
	}

	if mode == "rules" {
		text, kb := fwRulesView()
		_, err := m.Reply(text, &telegram.SendOptions{ReplyMarkup: kb})
		return err
	}

	dialogs, err := fwLoadDialogs(client, strings.TrimSpace(m.Args()))
	if err != nil {
		sendMS(m, fmt.Sprintf("读取聊天列表失败: %v", err), nil, 60)
		return nil
	}
	if len(dialogs) == 0 {
		sendMS(m, "没有找到匹配的会话, 换个关键词试试", nil, 60)
		return nil
	}
	s := &fwSession{Mode: mode, Step: "src", Dialogs: dialogs, Selected: map[int32]bool{}, Updated: time.Now()}
	fwSessionsMu.Lock()
	fwSessions[m.SenderID()] = s
	fwSessionsMu.Unlock()

	text, kb := fwDialogView(s)
	_, err = m.Reply(text, &telegram.SendOptions{ReplyMarkup: kb})
	return err
}

// handleFwTo 处理 /to 关键词: 在选目标这一步过滤会话
func handleFwTo(m *telegram.NewMessage) error {
	if !infos.needAdmin(m) {
		return nil
	}
	s := fwGetSession(m.SenderID())
	if s == nil || s.Step != "dst" {
		sendMS(m, "先用 /fw 选好来源和消息, 到「选目标」这一步再用 /to 关键词", nil, 60)
		return nil
	}
	client := infos.UserClient.Load()
	if client == nil {
		return nil
	}
	dialogs, err := fwLoadDialogs(client, strings.TrimSpace(m.Args()))
	if err != nil || len(dialogs) == 0 {
		sendMS(m, "没有找到匹配的会话", nil, 60)
		return nil
	}
	s.Dialogs, s.Page = dialogs, 0
	text, kb := fwDialogView(s)
	_, err = m.Reply(text, &telegram.SendOptions{ReplyMarkup: kb})
	return err
}

// ---------- 按钮回调 ----------

func handleFwCallback(q *telegram.CallbackQuery) error {
	if !infos.isAdmin(q.SenderID) {
		_, _ = q.Answer("你没有使用此功能的权限", &telegram.CallbackOptions{Alert: true})
		return nil
	}
	parts := strings.Split(q.DataString(), ":")
	if len(parts) < 2 {
		return nil
	}

	// 规则管理不依赖会话
	switch parts[1] {
	case "rdel":
		if len(parts) == 3 {
			if idx, err := strconv.Atoi(parts[2]); err == nil {
				fwDeleteRule(idx)
			}
		}
		text, kb := fwRulesView()
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
		_, _ = q.Answer("已删除")
		return nil
	case "radd":
		client := infos.UserClient.Load()
		if client == nil {
			_, _ = q.Answer("UserBot 未登录", &telegram.CallbackOptions{Alert: true})
			return nil
		}
		dialogs, err := fwLoadDialogs(client, "")
		if err != nil {
			_, _ = q.Answer("读取聊天列表失败", &telegram.CallbackOptions{Alert: true})
			return nil
		}
		s := &fwSession{Mode: "auto", Step: "src", Dialogs: dialogs, Selected: map[int32]bool{}, Updated: time.Now()}
		fwSessionsMu.Lock()
		fwSessions[q.SenderID] = s
		fwSessionsMu.Unlock()
		text, kb := fwDialogView(s)
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
		_, _ = q.Answer("")
		return nil
	}

	s := fwGetSession(q.SenderID)
	if s == nil {
		_, _ = q.Answer("这个菜单已过期, 请重新发送 /fw", &telegram.CallbackOptions{Alert: true})
		return nil
	}
	s.Updated = time.Now()
	client := infos.UserClient.Load()
	if client == nil {
		_, _ = q.Answer("UserBot 未登录", &telegram.CallbackOptions{Alert: true})
		return nil
	}

	arg := ""
	if len(parts) >= 3 {
		arg = parts[2]
	}

	switch parts[1] {
	case "x": // 取消
		fwSessionsMu.Lock()
		delete(fwSessions, q.SenderID)
		fwSessionsMu.Unlock()
		_, _ = q.Edit("已取消")
	case "p": // 翻页
		if page, err := strconv.Atoi(arg); err == nil {
			s.Page = page
		}
		text, kb := fwDialogView(s)
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
	case "d": // 选中某个会话
		var picked fwDialog
		if arg == "me" {
			picked = fwDialog{ID: 0, Name: "收藏夹", Kind: "⭐"}
		} else {
			idx, err := strconv.Atoi(arg)
			if err != nil || idx < 0 || idx >= len(s.Dialogs) {
				_, _ = q.Answer("选择无效")
				return nil
			}
			picked = s.Dialogs[idx]
		}
		switch {
		case s.Step == "src" && s.Mode == "auto":
			s.Source, s.Step, s.Page = picked, "dst", 0
			text, kb := fwDialogView(s)
			_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
		case s.Step == "src":
			s.Source = picked
			msgs, err := fwLoadMsgs(client, picked, 0, 0)
			if err != nil {
				_, _ = q.Answer(fmt.Sprintf("读取消息失败: %v", err), &telegram.CallbackOptions{Alert: true})
				return nil
			}
			s.Msgs, s.Step = msgs, "msg"
			text, kb := fwMsgView(s)
			_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
		case s.Step == "dst" && s.Mode == "auto":
			if fwPeerOf(picked) == fwPeerOf(s.Source) {
				_, _ = q.Answer("来源和目标不能是同一个会话", &telegram.CallbackOptions{Alert: true})
				return nil
			}
			if err := fwAddRule(fwRule{Source: s.Source, Target: picked}); err != nil {
				_, _ = q.Answer(err.Error(), &telegram.CallbackOptions{Alert: true})
				return nil
			}
			fwSessionsMu.Lock()
			delete(fwSessions, q.SenderID)
			fwSessionsMu.Unlock()
			text, kb := fwRulesView()
			_, _ = q.Edit("✅ 已添加规则\n\n"+text, &telegram.SendOptions{ReplyMarkup: kb})
		case s.Step == "dst":
			fwSessionsMu.Lock()
			delete(fwSessions, q.SenderID)
			fwSessionsMu.Unlock()
			fwRunOnce(q, client, s, picked)
		}
	case "m": // 勾选/取消勾选消息
		if id, err := strconv.ParseInt(arg, 10, 32); err == nil {
			mid := int32(id)
			s.Selected[mid] = !s.Selected[mid]
			if !s.Selected[mid] {
				delete(s.Selected, mid)
			}
		}
		text, kb := fwMsgView(s)
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
	case "all": // 全选本页
		for _, msg := range s.Msgs {
			s.Selected[msg.ID] = true
		}
		text, kb := fwMsgView(s)
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
	case "older", "newer":
		var offset, minID int32
		if len(s.Msgs) > 0 {
			if parts[1] == "older" {
				offset = s.Msgs[len(s.Msgs)-1].ID
			} else {
				minID = s.Msgs[0].ID
				offset = minID + fwMsgPage + 1
			}
		}
		msgs, err := fwLoadMsgs(client, s.Source, offset, minID)
		if err != nil || len(msgs) == 0 {
			_, _ = q.Answer("没有更多消息了")
			return nil
		}
		s.Msgs = msgs
		text, kb := fwMsgView(s)
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
	case "next": // 勾选完成, 去选目标
		if len(s.Selected) == 0 {
			_, _ = q.Answer("先勾选要转发的消息", &telegram.CallbackOptions{Alert: true})
			return nil
		}
		dialogs, err := fwLoadDialogs(client, "")
		if err == nil {
			s.Dialogs = dialogs
		}
		s.Step, s.Page = "dst", 0
		text, kb := fwDialogView(s)
		_, _ = q.Edit(text, &telegram.SendOptions{ReplyMarkup: kb})
	}
	_, _ = q.Answer("")
	return nil
}

// fwRunOnce 执行一次性转发
func fwRunOnce(q *telegram.CallbackQuery, client *telegram.Client, s *fwSession, target fwDialog) {
	ids := make([]int32, 0, len(s.Selected))
	for id := range s.Selected {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// 同一个相册只转一次(forwardByID 会自动带上整组)
	grouped := map[int32]int64{}
	for _, msg := range s.Msgs {
		grouped[msg.ID] = msg.Grouped
	}
	seen := map[int64]bool{}
	var todo []int32
	for _, id := range ids {
		if g := grouped[id]; g != 0 {
			if seen[g] {
				continue
			}
			seen[g] = true
		}
		todo = append(todo, id)
	}

	if !forwardMu.TryLock() {
		_, _ = q.Edit("已有转发任务在进行, 请稍后再试")
		return
	}
	_, _ = q.Edit(fmt.Sprintf("正在转发 %d 条: %s %s → %s %s ...", len(todo), s.Source.Kind, s.Source.Name, target.Kind, target.Name))

	source := fwPeerOf(s.Source)
	dst := fwPeerOf(target)
	safeGo(func() {
		defer forwardMu.Unlock()
		var done, failed int
		var lastErr error
		for _, id := range todo {
			if _, err := forwardByID(client, dst, source, id); err != nil {
				failed++
				lastErr = err
				log.Printf("转发失败 %v/%d: %+v", source, id, err)
				continue
			}
			done++
		}
		text := fmt.Sprintf("转发结束: %s %s → %s %s\n成功 %d 条, 失败 %d 条", s.Source.Kind, s.Source.Name, target.Kind, target.Name, done, failed)
		if lastErr != nil {
			text += fmt.Sprintf("\n最后一个错误: %v", lastErr)
		}
		_, _ = q.Edit(text)
	})
}

// ---------- 视图 ----------

func fwDialogView(s *fwSession) (string, *telegram.ReplyInlineMarkup) {
	var title string
	switch {
	case s.Mode == "auto" && s.Step == "src":
		title = "添加自动转发 ①/② 选来源会话(之后它的新消息都会被转发)"
	case s.Mode == "auto":
		title = fmt.Sprintf("添加自动转发 ②/② 选目标\n来源: %s %s", s.Source.Kind, s.Source.Name)
	case s.Step == "src":
		title = "转发 ①/③ 选来源会话\n(会话多可以用 /fw 关键词 搜索)"
	default:
		title = fmt.Sprintf("转发 ③/③ 选目标, 已选 %d 条\n来源: %s %s\n(可用 /to 关键词 搜索目标)", len(s.Selected), s.Source.Kind, s.Source.Name)
	}

	pages := (len(s.Dialogs) + fwPageSize - 1) / fwPageSize
	if s.Page >= pages {
		s.Page = max(0, pages-1)
	}
	kb := telegram.NewKeyboard()
	if s.Step == "dst" {
		kb.AddRow(telegram.Button.Data("⭐ 收藏夹", "fw:d:me"))
	}
	start := s.Page * fwPageSize
	end := min(start+fwPageSize, len(s.Dialogs))
	for i := start; i < end; i++ {
		d := s.Dialogs[i]
		kb.AddRow(telegram.Button.Data(fwCut(d.Kind+" "+d.Name, 40), fmt.Sprintf("fw:d:%d", i)))
	}
	var nav []telegram.KeyboardInlineButton
	if s.Page > 0 {
		nav = append(nav, telegram.Button.Data("◀ 上一页", fmt.Sprintf("fw:p:%d", s.Page-1)))
	}
	if s.Page < pages-1 {
		nav = append(nav, telegram.Button.Data("下一页 ▶", fmt.Sprintf("fw:p:%d", s.Page+1)))
	}
	nav = append(nav, telegram.Button.Data("✖ 取消", "fw:x"))
	kb.AddRow(nav...)
	return fmt.Sprintf("%s\n第 %d/%d 页", title, s.Page+1, max(1, pages)), kb.Build()
}

func fwMsgView(s *fwSession) (string, *telegram.ReplyInlineMarkup) {
	kb := telegram.NewKeyboard()
	for _, msg := range s.Msgs {
		box := "☐"
		if s.Selected[msg.ID] {
			box = "☑"
		}
		kb.AddRow(telegram.Button.Data(fwCut(box+" "+msg.Label, 48), fmt.Sprintf("fw:m:%d", msg.ID)))
	}
	kb.AddRow(
		telegram.Button.Data("⬅ 更早", "fw:older"),
		telegram.Button.Data("更新 ➡", "fw:newer"),
		telegram.Button.Data("全选本页", "fw:all"),
	)
	kb.AddRow(
		telegram.Button.Data(fmt.Sprintf("下一步(已选 %d)", len(s.Selected)), "fw:next"),
		telegram.Button.Data("✖ 取消", "fw:x"),
	)
	return fmt.Sprintf("转发 ②/③ 勾选消息(可翻页多选)\n来源: %s %s", s.Source.Kind, s.Source.Name), kb.Build()
}

func fwRulesView() (string, *telegram.ReplyInlineMarkup) {
	fwRulesMu.RLock()
	rules := append([]fwRule(nil), fwRules...)
	fwRulesMu.RUnlock()

	kb := telegram.NewKeyboard()
	var b strings.Builder
	b.WriteString("自动转发规则(来源有新消息就自动转到目标, 禁止转发的会话自动下载重传)\n")
	if len(rules) == 0 {
		b.WriteString("\n还没有规则")
	}
	for i, r := range rules {
		fmt.Fprintf(&b, "\n%d. %s %s → %s %s", i+1, r.Source.Kind, r.Source.Name, r.Target.Kind, r.Target.Name)
		kb.AddRow(telegram.Button.Data(fmt.Sprintf("🗑 删除第 %d 条", i+1), fmt.Sprintf("fw:rdel:%d", i)))
	}
	kb.AddRow(telegram.Button.Data("➕ 添加规则", "fw:radd"))
	return b.String(), kb.Build()
}

// ---------- 数据 ----------

func fwGetSession(uid int64) *fwSession {
	fwSessionsMu.Lock()
	defer fwSessionsMu.Unlock()
	for id, s := range fwSessions {
		if time.Since(s.Updated) > fwSessionTTL {
			delete(fwSessions, id)
		}
	}
	return fwSessions[uid]
}

// fwLoadDialogs 读取 UserBot 的聊天列表, 可按名字过滤
func fwLoadDialogs(client *telegram.Client, keyword string) ([]fwDialog, error) {
	raw, err := client.GetDialogs(&telegram.DialogOptions{Limit: fwDialogMax})
	if err != nil {
		return nil, err
	}
	keyword = strings.ToLower(strings.TrimPrefix(keyword, "@"))
	var out []fwDialog
	for i := range raw {
		d := fwDescribe(client, &raw[i])
		if d.ID == 0 {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(d.Name), keyword) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func fwDescribe(client *telegram.Client, d *telegram.TLDialog) fwDialog {
	out := fwDialog{ID: d.GetChannelID()}
	switch {
	case d.IsUser():
		out.Kind = "👤"
		if u, err := d.GetUser(client); err == nil && u != nil {
			if u.Bot {
				out.Kind = "🤖"
			}
			if u.Self {
				return fwDialog{} // 收藏夹单独提供按钮
			}
			out.Name = strings.TrimSpace(u.FirstName + " " + u.LastName)
			if out.Name == "" {
				out.Name = u.Username
			}
		}
	case d.IsChat():
		out.Kind = "👥"
		if c, err := d.GetChat(client); err == nil && c != nil {
			out.Name = c.Title
		}
	case d.IsChannel():
		out.Kind = "👥"
		if c, err := d.GetChannel(client); err == nil && c != nil {
			if c.Broadcast {
				out.Kind = "📢"
			}
			out.Name = c.Title
		}
	}
	if out.Name == "" {
		out.Name = strconv.FormatInt(out.ID, 10)
	}
	return out
}

// fwPeerOf 把会话转成 ResolvePeer 认得的参数
func fwPeerOf(d fwDialog) any {
	if d.ID == 0 {
		return "me"
	}
	return d.ID
}

// fwLoadMsgs 读取一页消息(从新到旧), offset 为 0 表示最新
func fwLoadMsgs(client *telegram.Client, d fwDialog, offset, minID int32) ([]fwMsg, error) {
	ms, err := client.GetHistory(fwPeerOf(d), &telegram.HistoryOption{Limit: fwMsgPage, Offset: offset, MinID: minID})
	if err != nil {
		return nil, err
	}
	out := make([]fwMsg, 0, len(ms))
	for _, msg := range ms {
		if msg.Message == nil {
			continue
		}
		out = append(out, fwMsg{ID: msg.ID, Grouped: msg.Message.GroupedID, Label: fwLabel(msg)})
	}
	return out, nil
}

func fwLabel(msg telegram.NewMessage) string {
	text := strings.Join(strings.Fields(msg.Message.Message), " ")
	icon := "💬"
	switch {
	case msg.Video() != nil:
		icon = "🎬"
	case msg.Photo() != nil:
		icon = "🖼"
	case msg.Document() != nil:
		icon = "📎"
		if text == "" {
			text = mediaName(msg)
		}
	case msg.IsMedia():
		icon = "📦"
	}
	if msg.Message.GroupedID != 0 {
		icon += "[组]"
	}
	if text == "" {
		text = "(无文字)"
	}
	return fmt.Sprintf("%s %s", icon, text)
}

func fwCut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// ---------- 自动转发 ----------

func fwRulesPath() string { return filepath.Join(infos.FilesPath, fwRulesFile) }

func fwLoadRules() {
	data, err := os.ReadFile(fwRulesPath())
	if err != nil {
		return
	}
	var rules []fwRule
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Printf("读取自动转发规则失败: %+v", err)
		return
	}
	fwRulesMu.Lock()
	fwRules = rules
	fwRulesMu.Unlock()
}

func fwSaveRules() {
	fwRulesMu.RLock()
	data, _ := json.MarshalIndent(fwRules, "", "  ")
	fwRulesMu.RUnlock()
	if err := os.WriteFile(fwRulesPath(), data, 0600); err != nil {
		log.Printf("保存自动转发规则失败: %+v", err)
	}
}

func fwAddRule(r fwRule) error {
	fwRulesMu.Lock()
	for _, old := range fwRules {
		if old.Source.ID == r.Source.ID && old.Target.ID == r.Target.ID {
			fwRulesMu.Unlock()
			return nil
		}
		// 不允许链式/环形规则(A→B 且 B→A 会无限互转)
		if old.Source.ID == r.Target.ID || old.Target.ID == r.Source.ID {
			fwRulesMu.Unlock()
			return fmt.Errorf("和已有规则 %s→%s 首尾相连, 可能造成循环转发", old.Source.Name, old.Target.Name)
		}
	}
	fwRules = append(fwRules, r)
	fwRulesMu.Unlock()
	fwSaveRules()
	return nil
}

func fwDeleteRule(idx int) {
	fwRulesMu.Lock()
	if idx >= 0 && idx < len(fwRules) {
		fwRules = append(fwRules[:idx], fwRules[idx+1:]...)
	}
	fwRulesMu.Unlock()
	fwSaveRules()
}

type fwAlbumBuf struct {
	timer *time.Timer
}

// handleAutoForward 挂在 UserBot 上: 来源会话出现新消息时按规则转发
func handleAutoForward(m *telegram.NewMessage) error {
	if m == nil || m.Message == nil {
		return nil
	}
	chat := m.ChannelID()
	fwRulesMu.RLock()
	var targets []fwDialog
	for _, r := range fwRules {
		if r.Source.ID == chat {
			targets = append(targets, r.Target)
		}
	}
	fwRulesMu.RUnlock()
	if len(targets) == 0 {
		return nil
	}
	client := m.Client

	run := func() {
		fwAutoMu.Lock() // 自动转发排队执行, 避免多个大文件同时下载
		defer fwAutoMu.Unlock()
		for _, t := range targets {
			if fwPeerOf(t) == any(chat) {
				continue
			}
			if mode, err := forwardByID(client, fwPeerOf(t), chat, m.ID); err != nil {
				log.Printf("自动转发失败 %d/%d -> %v: %+v", chat, m.ID, t.ID, err)
			} else if infos.Conf.Load().DeBUG {
				log.Printf("自动转发 %d/%d -> %v (%s)", chat, m.ID, t.ID, mode)
			}
		}
	}

	// 相册的每一项会分别到达, 等 3 秒凑齐后只转一次(forwardByID 自动取整组)
	if g := m.Message.GroupedID; g != 0 {
		fwAlbumsMu.Lock()
		if _, ok := fwAlbums[g]; ok {
			fwAlbumsMu.Unlock()
			return nil
		}
		fwAlbums[g] = &fwAlbumBuf{timer: time.AfterFunc(3*time.Second, func() {
			fwAlbumsMu.Lock()
			delete(fwAlbums, g)
			fwAlbumsMu.Unlock()
			run()
		})}
		fwAlbumsMu.Unlock()
		return nil
	}
	safeGo(run)
	return nil
}

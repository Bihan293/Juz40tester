package handlers

import (
	"context"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// Callback data prefixes (kept short — Telegram limit is 64 bytes).
const (
	cbSubjects    = "subj:list"
	cbSubject     = "subj:open:"    // + subjectID
	cbSubjectPage = "subj:page:"    // + subjectID:page
	cbOpenTest    = "test:open:"    // + testID (opens the test straight away)
	cbPendingID   = "test:pending:" // + subjectID:testNumber — tap re-queues a stuck/failed generation
	cbWeakMenu    = "weak:menu"     // weak-topics subject picker
	cbWeakSubject = "weak:subj:"    // + subjectID (open/generate the personal weak test)
	cbFinish      = "test:finish:"  // + testID (finish & delete a mastered personal test)
	cbAnswer      = "ans:"          // + attemptID:position:displayIndex
	cbExit        = "test:exit:"    // + attemptID (asks for confirmation)
	cbExitYes     = "test:exit:y:"  // + attemptID (confirmed exit)
	cbExitNo      = "test:exit:n:"  // + attemptID (cancel — back to the question)
	cbRetry       = "test:retry:"   // + attemptID (same test again)
	cbProgress    = "nav:progress"
	cbProgSubject = "prog:subj:" // + subjectID
	cbSettings    = "nav:settings"
	cbSetLang     = "settings:lang:" // + "ru" | "kk"
	cbMainMenu    = "nav:menu"
	cbNoop        = "noop"
	cbLeaderboard = "lb:menu"   // leaderboard subject picker
	cbLbSubject   = "lb:subj:"  // + subjectID (levels/green board of a subject)
	cbLbStreak    = "lb:streak" // 🔥 streak board
)

// --- Messages ---------------------------------------------------------------

// commandKey normalises a Telegram command for routing: "/start payload"
// (deep link), "/start@MyBot" and "/start@MyBot payload" all become
// "/start". Non-command text (reply-keyboard buttons) is returned as is.
func commandKey(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	cmd := text
	if i := strings.IndexAny(cmd, " \t\n"); i >= 0 {
		cmd = cmd[:i]
	}
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
}

func (h *Handler) handleMessage(ctx context.Context, m *bot.Message) {
	// The bot is a personal tutor: it only talks in private chats. If it is
	// ever added to a group, it must not answer every group message with the
	// main menu (and must not register every group member as a user).
	if m.Chat.Type != "" && m.Chat.Type != "private" {
		return
	}
	// R-9: message spam (e.g. a held-down /start) is silently ignored.
	if h.throttled(m.From.ID) {
		return
	}
	user, err := h.ensureUser(ctx, m.From)
	if err != nil {
		logf("upsert user %d: %v", m.From.ID, err)
		return
	}

	text := strings.TrimSpace(m.Text)
	// Admin panel: /admin, «🛠 Админка» and the input of an admin dialog
	// step (search query, broadcast text / media / buttons). Only for
	// ADMIN_IDS — for everybody else this is a no-op.
	if h.handleAdminMessage(ctx, m, user) {
		return
	}
	if strings.HasPrefix(text, "/") && h.handleAdmin(ctx, m, text) {
		return
	}
	cmd := commandKey(text)
	if h.custom != nil && isMenuCommand(cmd) {
		// Any menu button / command leaves the «send the description» step.
		if err := h.custom.ClearDraft(ctx, user.ID); err != nil {
			logf("custom clear draft %d: %v", user.ID, err)
		}
	}
	switch cmd {
	case "/start":
		h.sendMainMenu(ctx, m.Chat.ID, user, true)
	case kbSubjects, "/subjects":
		h.showSubjects(ctx, m.Chat.ID)
	case kbWeak, "/weak":
		h.showWeakMenu(ctx, m.Chat.ID, user)
	case kbCustom, "/custom":
		h.showCustomMenu(ctx, m.Chat.ID, user)
	case kbProgress, "/progress":
		h.showProgress(ctx, m.Chat.ID, user)
	case kbTop, "/top":
		h.showLeaderboardMenu(ctx, m.Chat.ID)
	case kbSettings, "/settings":
		h.showSettings(ctx, m.Chat.ID, user)
	case kbPlans, "/plans", "/subscription", "/tariffs":
		h.showPlans(ctx, m.Chat.ID, user)
	default:
		// The description of a «✨ Свой тест» test (the user is in that step).
		if !strings.HasPrefix(text, "/") && h.handleCustomDescription(ctx, m, user, text) {
			return
		}
		// Unknown text/command -> main menu.
		h.sendMainMenu(ctx, m.Chat.ID, user, false)
	}
}

// isMenuCommand reports a reply-keyboard button or a known command.
func isMenuCommand(cmd string) bool {
	switch cmd {
	case "/start", kbSubjects, "/subjects", kbWeak, "/weak", kbCustom, "/custom", kbProgress, "/progress",
		kbTop, "/top", kbSettings, "/settings", kbPlans, "/plans", "/subscription", "/tariffs":
		return true
	}
	return false
}

// --- Callbacks router -----------------------------------------------------------

func (h *Handler) handleCallback(ctx context.Context, cb *bot.CallbackQuery) {
	// R-9: a tap within UserActionInterval of the previous one is only
	// acknowledged (stops the spinner) — no DB query, no other Telegram
	// call. Duplicate answer taps are thus dropped before SubmitAnswer
	// (which stays idempotent on its own anyway).
	// Private chats only (like handleMessage): callbacks from a group are
	// ignored — no user registration, no DB work.
	if cb.Message != nil && cb.Message.Chat.Type != "" && cb.Message.Chat.Type != "private" {
		return
	}
	if h.throttled(cb.From.ID) {
		if err := h.tg.AnswerCallbackQuery(ctx, cb.ID, ""); err != nil {
			logf("answer throttled callback: %v", err)
		}
		return
	}
	user, err := h.ensureUser(ctx, cb.From)
	if err != nil {
		logf("upsert user %d: %v", cb.From.ID, err)
		return
	}
	data := cb.Data
	defer h.answered.Delete(cb.ID)
	// Very old / inaccessible messages arrive without cb.Message — every
	// handler below dereferences cb.Message.Chat.ID, so bail out safely.
	if cb.Message == nil {
		h.answerCallback(ctx, cb, "Сообщение устарело — открой меню заново")
		return
	}

	// IMPORTANT: a callback_query may be answered EXACTLY ONCE — a second
	// answerCallbackQuery call is rejected by Telegram, which silently
	// breaks every notification (the "Чтобы открыть Тест N…", «Минуточку,
	// тест генерируется…» popups never showed because the router answered
	// the callback BEFORE the handlers did). The router therefore answers
	// nothing; each handler acknowledges the callback itself.
	if strings.HasPrefix(data, cbAdmPrefix) {
		// Admin panel: re-checked against ADMIN_IDS inside (a forged
		// button of a non-admin is only acknowledged).
		h.handleAdminCallback(ctx, cb, user, data)
		return
	}
	if h.routeByID(ctx, cb, user, data) {
		return
	}
	switch {
	case data == cbNoop:
		h.answerCallback(ctx, cb, "")
	case data == cbMainMenu:
		h.answerCallback(ctx, cb, "")
		h.sendMainMenu(ctx, cb.Message.Chat.ID, user, false)
	case data == cbSubjects:
		h.answerCallback(ctx, cb, "")
		h.editSubjects(ctx, cb)
	case strings.HasPrefix(data, cbSubjectPage):
		h.answerCallback(ctx, cb, "")
		h.flipSubjectPage(ctx, cb, user, data)
	case strings.HasPrefix(data, cbPendingID):
		h.handlePendingTest(ctx, cb, user, data)
	case data == cbWeakMenu:
		h.answerCallback(ctx, cb, "")
		h.editWeakMenu(ctx, cb, user)
	case data == cbCustomMenu:
		h.editCustomMenu(ctx, cb, user) // answers itself
	case strings.HasPrefix(data, cbAnswer):
		h.handleAnswer(ctx, cb, user, data)
	case data == cbProgress:
		h.answerCallback(ctx, cb, "")
		h.editProgress(ctx, cb, user)
	case data == cbSettings:
		h.answerCallback(ctx, cb, "")
		h.editSettings(ctx, cb, user)
	case strings.HasPrefix(data, cbSetLang):
		lang := strings.TrimPrefix(data, cbSetLang)
		h.setTestLang(ctx, cb, user, lang)
	case data == cbLeaderboard:
		h.answerCallback(ctx, cb, "")
		h.editLeaderboardMenu(ctx, cb)
	case data == cbPlans:
		h.editPlans(ctx, cb, user)
	case strings.HasPrefix(data, cbBuyPlan):
		h.buyPlan(ctx, cb, user, strings.TrimPrefix(data, cbBuyPlan))
	case data == cbLbStreak:
		h.answerCallback(ctx, cb, "")
		h.showStreakLeaderboard(ctx, cb)
	default:
		h.answerCallback(ctx, cb, "")
	}
}

// idRoute is a callback whose data is prefix + numeric id (A7). ack = the
// router acknowledges the callback (silently) before calling fn; otherwise
// fn answers the callback itself. A malformed id is acknowledged silently
// and nothing else happens — the behaviour of the former switch cases.
type idRoute struct {
	prefix string
	ack    bool
	fn     func(h *Handler, ctx context.Context, cb *bot.CallbackQuery, user *models.User, id int64)
}

// idRoutes: a new numeric-id section is one line here. Order matters where
// prefixes overlap: cbExitYes / cbExitNo must precede cbExit.
var idRoutes = []idRoute{
	{cbWeakSubject, false, (*Handler).openWeakSubject},     // answers itself (toast / error / silent)
	{cbWeakBuy, false, (*Handler).buyWeakTest},             // answers itself
	{cbFinish, false, (*Handler).finishPersonalTest},       // answers itself
	{cbCustomSubject, false, (*Handler).openCustomSubject}, // answers itself
	{cbCustomNew, false, (*Handler).newCustomTest},         // answers itself (alerts)
	{cbCustomFinish, false, (*Handler).finishCustomTest},   // answers itself
	{cbCustomCancel, false, (*Handler).cancelCustomDraft},  // answers itself
	{cbCustomFree, false, (*Handler).confirmFreeCustom},    // answers itself
	{cbSubject, true, (*Handler).openSubject},              //
	{cbOpenTest, false, (*Handler).openTest},               // answers itself (unlock requirements toast)
	{cbExitYes, false, (*Handler).exitTest},                // answers itself («Прогресс сохранён»)
	{cbExitNo, false, (*Handler).cancelExit},               // answers itself («Продолжаем 💪»)
	{cbExit, true, (*Handler).confirmExit},                 //
	{cbRetry, true, (*Handler).retryTest},                  //
	{cbProgSubject, true, (*Handler).showSubjectProgress},  //
	{cbLbSubject, true, (*Handler).showSubjectLeaderboardFor},
}

// matchIDRoute finds the route of data and parses its id (r == nil: data is
// not a numeric-id callback).
func matchIDRoute(data string) (r *idRoute, id int64, err error) {
	for i := range idRoutes {
		if strings.HasPrefix(data, idRoutes[i].prefix) {
			id, err = strconv.ParseInt(strings.TrimPrefix(data, idRoutes[i].prefix), 10, 64)
			return &idRoutes[i], id, err
		}
	}
	return nil, 0, nil
}

// routeByID dispatches a numeric-id callback; false when data is not one.
func (h *Handler) routeByID(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) bool {
	r, id, err := matchIDRoute(data)
	if r == nil {
		return false
	}
	if r.ack || err != nil {
		h.answerCallback(ctx, cb, "")
	}
	if err == nil {
		r.fn(h, ctx, cb, user, id)
	}
	return true
}

// showSubjectLeaderboardFor adapts showSubjectLeaderboard to idRoute.fn.
func (h *Handler) showSubjectLeaderboardFor(ctx context.Context, cb *bot.CallbackQuery, _ *models.User, id int64) {
	h.showSubjectLeaderboard(ctx, cb, id)
}

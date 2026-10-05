package handlers

import (
	"context"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/bot"
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
	switch commandKey(text) {
	case "/start":
		h.sendMainMenu(ctx, m.Chat.ID, user, true)
	case kbSubjects, "/subjects":
		h.showSubjects(ctx, m.Chat.ID)
	case kbWeak, "/weak":
		h.showWeakMenu(ctx, m.Chat.ID, user)
	case kbProgress, "/progress":
		h.showProgress(ctx, m.Chat.ID, user)
	case kbTop, "/top":
		h.showLeaderboardMenu(ctx, m.Chat.ID)
	case kbSettings, "/settings":
		h.showSettings(ctx, m.Chat.ID, user)
	default:
		// Unknown text/command -> main menu.
		h.sendMainMenu(ctx, m.Chat.ID, user, false)
	}
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
	case strings.HasPrefix(data, cbWeakSubject):
		// openWeakSubject answers the callback itself (the result decides the
		// text: a toast, an error, or a silent transition into the test).
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbWeakSubject), 10, 64)
		if err != nil {
			h.answerCallback(ctx, cb, "")
			return
		}
		h.openWeakSubject(ctx, cb, user, id)
	case strings.HasPrefix(data, cbFinish):
		// finishPersonalTest answers the callback itself.
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbFinish), 10, 64)
		if err != nil {
			h.answerCallback(ctx, cb, "")
			return
		}
		h.finishPersonalTest(ctx, cb, user, id)
	case strings.HasPrefix(data, cbSubject):
		h.answerCallback(ctx, cb, "")
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbSubject), 10, 64)
		if err != nil {
			return
		}
		h.openSubject(ctx, cb, user, id)
	case strings.HasPrefix(data, cbOpenTest):
		// openTest answers the callback itself: a locked test shows the
		// unlock requirements as a toast — the router must not consume the
		// single allowed answer with an empty one first.
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbOpenTest), 10, 64)
		if err != nil {
			h.answerCallback(ctx, cb, "")
			return
		}
		h.openTest(ctx, cb, user, id)
	case strings.HasPrefix(data, cbAnswer):
		h.handleAnswer(ctx, cb, user, data)
	case strings.HasPrefix(data, cbExitYes):
		// exitTest answers the callback itself ("Прогресс сохранён").
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbExitYes), 10, 64)
		if err != nil {
			h.answerCallback(ctx, cb, "")
			return
		}
		h.exitTest(ctx, cb, user, id)
	case strings.HasPrefix(data, cbExitNo):
		// cancelExit answers the callback itself ("Продолжаем 💪").
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbExitNo), 10, 64)
		if err != nil {
			h.answerCallback(ctx, cb, "")
			return
		}
		h.cancelExit(ctx, cb, user, id)
	case strings.HasPrefix(data, cbExit):
		h.answerCallback(ctx, cb, "")
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbExit), 10, 64)
		if err != nil {
			return
		}
		h.confirmExit(ctx, cb, user, id)
	case strings.HasPrefix(data, cbRetry):
		h.answerCallback(ctx, cb, "")
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbRetry), 10, 64)
		if err != nil {
			return
		}
		h.retryTest(ctx, cb, user, id)
	case data == cbProgress:
		h.answerCallback(ctx, cb, "")
		h.editProgress(ctx, cb, user)
	case strings.HasPrefix(data, cbProgSubject):
		h.answerCallback(ctx, cb, "")
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbProgSubject), 10, 64)
		if err != nil {
			return
		}
		h.showSubjectProgress(ctx, cb, user, id)
	case data == cbSettings:
		h.answerCallback(ctx, cb, "")
		h.editSettings(ctx, cb, user)
	case strings.HasPrefix(data, cbSetLang):
		lang := strings.TrimPrefix(data, cbSetLang)
		h.setTestLang(ctx, cb, user, lang)
	case data == cbLeaderboard:
		h.answerCallback(ctx, cb, "")
		h.editLeaderboardMenu(ctx, cb)
	case data == cbLbStreak:
		h.answerCallback(ctx, cb, "")
		h.showStreakLeaderboard(ctx, cb)
	case strings.HasPrefix(data, cbLbSubject):
		h.answerCallback(ctx, cb, "")
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbLbSubject), 10, 64)
		if err != nil {
			return
		}
		h.showSubjectLeaderboard(ctx, cb, id)
	default:
		h.answerCallback(ctx, cb, "")
	}
}

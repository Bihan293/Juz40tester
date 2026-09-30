// Package handlers routes Telegram updates to business logic and renders UI.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40-test2/internal/bot"
	"github.com/Bihan293/Juz40-test2/internal/models"
	"github.com/Bihan293/Juz40-test2/internal/repositories"
	"github.com/Bihan293/Juz40-test2/internal/services"
)

// Reply-keyboard (bottom of chat) button texts.
const (
	kbSubjects = "📚 Предметы"
	kbWeak     = "🎯 Слабые темы"
	kbProgress = "📊 Статистика"
	kbTop      = "🏆 Топ"
	kbSettings = "⚙️ Настройки"
)

// Callback data prefixes (kept short — Telegram limit is 64 bytes).
const (
	cbSubjects    = "subj:list"
	cbSubject     = "subj:open:"    // + subjectID
	cbSubjectPage = "subj:page:"    // + subjectID:page
	cbOpenTest    = "test:open:"    // + testID (opens the test straight away)
	cbPending     = "test:pending"  // legacy: test is being generated (toast only)
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

// Handler wires the Telegram client to the services.
type Handler struct {
	tg    *bot.Client
	users *repositories.UserRepository
	quiz  *services.QuizService
	// watchers tracks the active «wait until the test is generated»
	// goroutines (key -> struct{}), so repeated taps on the same ⏳ button
	// never spawn duplicate watchers or flood the chat with notes.
	watchers sync.Map
}

// New creates a Handler.
func New(tg *bot.Client, users *repositories.UserRepository, quiz *services.QuizService) *Handler {
	return &Handler{tg: tg, users: users, quiz: quiz}
}

// HandleUpdate processes a single webhook update. It never panics; errors are logged.
func (h *Handler) HandleUpdate(ctx context.Context, upd *bot.Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in update %d: %v", upd.UpdateID, r)
		}
	}()
	switch {
	case upd.Message != nil && upd.Message.From != nil:
		h.handleMessage(ctx, upd.Message)
	case upd.CallbackQuery != nil && upd.CallbackQuery.From != nil:
		h.handleCallback(ctx, upd.CallbackQuery)
	}
}

// ensureUser registers or refreshes the Telegram user.
func (h *Handler) ensureUser(ctx context.Context, from *bot.TgUser) (*models.User, error) {
	return h.users.Upsert(ctx, &models.User{
		TelegramID:   from.ID,
		Username:     from.Username,
		FirstName:    from.FirstName,
		LastName:     from.LastName,
		LanguageCode: from.LanguageCode,
	})
}

// --- Messages ---------------------------------------------------------------

func (h *Handler) handleMessage(ctx context.Context, m *bot.Message) {
	user, err := h.ensureUser(ctx, m.From)
	if err != nil {
		log.Printf("upsert user %d: %v", m.From.ID, err)
		return
	}

	text := strings.TrimSpace(m.Text)
	switch text {
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

// --- Main menu ---------------------------------------------------------------

// mainMenuKeyboard is the persistent Reply Keyboard at the bottom of the chat:
// Предметы · Слабые темы / Статистика · Топ / Настройки.
func mainMenuKeyboard() *bot.ReplyKeyboardMarkup {
	return &bot.ReplyKeyboardMarkup{
		ResizeKeyboard: true,
		Keyboard: [][]bot.KeyboardButton{
			bot.ReplyRow(kbSubjects, kbWeak),
			bot.ReplyRow(kbProgress, kbTop),
			bot.ReplyRow(kbSettings),
		},
	}
}

// hideReplyKeyboard sends a tiny message with ReplyKeyboardRemove so the
// bottom menu (Предметы / Слабые темы / Статистика / Настройки) disappears
// while a test is in progress and cannot distract or break the flow.
func (h *Handler) hideReplyKeyboard(ctx context.Context, chatID int64) {
	msgID, err := h.tg.SendMessage(ctx, chatID, "✍️ Идёт тест — меню скрыто до конца. Вопросы ниже 👇", bot.RemoveKeyboard)
	if err != nil {
		log.Printf("hide reply keyboard: %v", err)
		return
	}
	// The message is only the keyboard-removal vehicle — delete it so the
	// chat stays clean. The keyboard stays hidden after the deletion.
	if err := h.tg.DeleteMessage(ctx, chatID, msgID); err != nil {
		log.Printf("delete keyboard-removal note: %v", err)
	}
}

// restoreReplyKeyboard brings the bottom main menu back once the test is
// over (finished or exited) — it was hidden while the test ran.
//
// BUG FIX: the menu used to be restored by a service message that was
// deleted right away. Telegram clients drop a reply keyboard together with
// the message that carried it, so after a test the main menu never came
// back (only the inline «⬅️ Главное меню» button helped, «📚 К предметам»
// left the user stuck without a menu). The note now STAYS in the chat, so
// the keyboard reliably reappears.
func (h *Handler) restoreReplyKeyboard(ctx context.Context, chatID int64) {
	if _, err := h.tg.SendMessage(ctx, chatID, "🏠 Главное меню снова доступно — кнопки внизу 👇", mainMenuKeyboard()); err != nil {
		log.Printf("restore reply keyboard: %v", err)
	}
}

func (h *Handler) sendMainMenu(ctx context.Context, chatID int64, user *models.User, greet bool) {
	text := fmt.Sprintf("Главное меню JUZ40 Tester. %s\nВыберите раздел кнопками ниже 👇", streakBadge(user))
	if greet {
		name := user.FirstName
		if name == "" {
			name = user.Username
		}
		if name == "" {
			name = "друг"
		}
		text = fmt.Sprintf("👋 Привет, %s! %s\n\nДобро пожаловать в JUZ40 Tester — бот для подготовки к ЕНТ.\nВыберите раздел кнопками ниже 👇", name, streakBadge(user))
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, mainMenuKeyboard()); err != nil {
		log.Printf("send main menu: %v", err)
	}
}

// streakBadge renders the daily-streak flame for the main menu: from the
// second consecutive day the 🔥 appears with the day count; on day one a
// soft nudge invites the user to come back tomorrow to light it up.
func streakBadge(user *models.User) string {
	switch {
	case user == nil || user.StreakDays <= 0:
		return ""
	case user.StreakDays == 1:
		return "Зайди завтра — и загорится твой огонёк 🔥"
	default:
		return fmt.Sprintf("🔥 %d %s подряд!", user.StreakDays, dayWord(user.StreakDays))
	}
}

// dayWord returns the correct Russian plural of «день».
func dayWord(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 14:
		return "дней"
	}
	switch n % 10 {
	case 1:
		return "день"
	case 2, 3, 4:
		return "дня"
	default:
		return "дней"
	}
}

// --- Subjects -----------------------------------------------------------------

func (h *Handler) renderSubjects(ctx context.Context, chatID int64) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}
	// Subject buttons — one per row so long names ("Математика",
	// "Грамотность чтения") fit and stay readable. Each subject gets its
	// own emoji to avoid confusion.
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	// A way home from the subjects list: without it a user who left a test
	// via «📚 К предметам» had no path back to the main menu.
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return "📚 Выберите предмет:", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// subjectEmoji picks a distinct emoji for a subject by its name so the
// subjects are easy to tell apart (falls back to 📚 for unknown names).
func subjectEmoji(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "математика", "математическая грамотность":
		return "📐"
	case "грамотность чтения", "чтение":
		return "📖"
	case "физика":
		return "⚛️"
	case "химия":
		return "🧪"
	case "биология":
		return "🧬"
	case "география":
		return "🌍"
	case "история", "история казахстана", "всемирная история":
		return "🏛️"
	case "казахский язык", "казахский язык и литература":
		return "🇰🇿"
	case "русский язык", "русский язык и литература":
		return "✏️"
	case "английский язык", "английский":
		return "🇬🇧"
	case "информатика":
		return "💻"
	default:
		return "📚"
	}
}

// showSubjects is used from the Reply Keyboard (new message).
func (h *Handler) showSubjects(ctx context.Context, chatID int64) {
	text, kb, err := h.renderSubjects(ctx, chatID)
	if err != nil {
		log.Printf("list subjects: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки предметов 😔", nil); err != nil {
			log.Printf("send subjects error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		log.Printf("send subjects: %v", err)
	}
}

// editSubjects is used from an inline button (edits the current message).
func (h *Handler) editSubjects(ctx context.Context, cb *bot.CallbackQuery) {
	text, kb, err := h.renderSubjects(ctx, cb.Message.Chat.ID)
	if err != nil {
		log.Printf("list subjects: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки предметов")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// renderSubject renders the tests grid of a subject: 3 buttons per row,
// paginated (⬅️/➡️), with 🔒 locked, ⏳ generating, ✅ completed and
// ⏸ resumable marks. The page is remembered per user.
func (h *Handler) renderSubject(ctx context.Context, user *models.User, subjectID int64, page int, remember bool) (string, *bot.InlineKeyboardMarkup, error) {
	scr, err := h.quiz.GetSubjectScreen(ctx, user.ID, subjectID, page, remember)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", subjectEmoji(scr.Subject.Name), scr.Subject.Name)
	fmt.Fprintf(&b, "Открыто тестов: %d · страница %d/%d\n\n", scr.UnlockedMax, scr.Page+1, scr.TotalPages)
	b.WriteString("Чтобы открыть следующий тест, доведи предыдущий до ")
	fmt.Fprintf(&b, "%d🟢 + %d🟡", models.UnlockGreen, models.UnlockYellow)
	b.WriteString(" — тогда я сразу начну его собирать ⏳")

	// Tests grid: 3 per row.
	buttons := make([]bot.InlineKeyboardButton, 0, len(scr.Slots))
	for _, slot := range scr.Slots {
		label := slot.Label
		data := ""
		switch {
		case slot.Test == nil && slot.Pending:
			label = "⏳ " + label
			// The button carries the coordinates of the missing test: tapping it
			// revives a failed/stuck generation job instead of just showing a toast.
			data = cbPendingID + strconv.FormatInt(subjectID, 10) + ":" + strconv.Itoa(slot.Number)
		case slot.Test == nil:
			label = "🔒 " + label
			data = cbNoop
		case !slot.Unlocked:
			label = "🔒 " + label
			data = cbOpenTest + strconv.FormatInt(slot.Test.ID, 10) // tap shows the requirement
		default:
			if slot.Completed {
				label = "✅ " + label
			} else if slot.Resume {
				label = "⏸ " + label
			}
			data = cbOpenTest + strconv.FormatInt(slot.Test.ID, 10)
		}
		buttons = append(buttons, bot.Btn(label, data))
	}
	rows := bot.ChunkButtons(buttons, models.TestsGridColumns)

	// Pagination row (only when there is more than one page).
	if scr.TotalPages > 1 {
		nav := make([]bot.InlineKeyboardButton, 0, 3)
		if scr.Page > 0 {
			nav = append(nav, bot.Btn("⬅️ Назад", fmt.Sprintf("%s%d:%d", cbSubjectPage, subjectID, scr.Page-1)))
		}
		nav = append(nav, bot.Btn(fmt.Sprintf("%d/%d", scr.Page+1, scr.TotalPages), cbNoop))
		if scr.Page < scr.TotalPages-1 {
			nav = append(nav, bot.Btn("Вперёд ➡️", fmt.Sprintf("%s%d:%d", cbSubjectPage, subjectID, scr.Page+1)))
		}
		rows = append(rows, nav)
	}

	rows = append(rows, bot.Row(
		bot.Btn("⬅️ К предметам", cbSubjects),
		bot.Btn("🏠 Главное меню", cbMainMenu),
	))
	return b.String(), &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (h *Handler) openSubject(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	// Restore the page where the user stopped last time.
	page, err := h.quiz.SavedTestsPage(ctx, user.ID, subjectID)
	if err != nil {
		log.Printf("saved page %d/%d: %v", user.ID, subjectID, err)
		page = 0
	}
	text, kb, err := h.renderSubject(ctx, user, subjectID, page, false)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Предмет не найден")
		return
	}
	if err != nil {
		log.Printf("subject info %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки предмета")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// handlePendingTest handles a tap on a ⏳ (still generating) test button.
// It REVIVES the generation (a failed/stuck job is re-queued as URGENT) and,
// instead of a tiny toast that vanished after a few seconds, sends a clear
// chat message «⏳ Тест генерируется… никуда не уходи». A background watcher
// then edits that message into «✅ Тест готов» with a start button (or an
// honest error with a retry button) — the user is never left guessing.
func (h *Handler) handlePendingTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	parts := strings.Split(strings.TrimPrefix(data, cbPendingID), ":")
	if len(parts) != 2 {
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Подожди минуточку и открой предмет снова.")
		return
	}
	subjectID, err1 := strconv.ParseInt(parts[0], 10, 64)
	testNumber, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Подожди минуточку и открой предмет снова.")
		return
	}
	h.quiz.ReviveChainTest(ctx, subjectID, testNumber, user.ID)

	// The test may have been generated a moment ago — open it right away.
	if test, _, err := h.quiz.ChainTestStatus(ctx, subjectID, testNumber); err == nil && test != nil {
		h.openTest(ctx, cb, user, test.ID)
		return
	}

	key := fmt.Sprintf("chain:%d:%d:%d", user.ID, subjectID, testNumber)
	check := func(ctx context.Context) (*models.Test, bool, error) {
		return h.quiz.ChainTestStatus(ctx, subjectID, testNumber)
	}
	retryData := cbPendingID + strconv.FormatInt(subjectID, 10) + ":" + strconv.Itoa(testNumber)
	note := fmt.Sprintf("⏳ «Тест %d» сейчас генерируется…\n\nНикуда не уходи, подожди минуточку — как только тест будет готов, я сразу пришлю сюда кнопку, чтобы его начать 👇", testNumber)
	h.startGenerationWatch(ctx, cb, key, note, check, retryData)
}

// flipSubjectPage switches the tests-grid page and remembers it.
func (h *Handler) flipSubjectPage(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	parts := strings.Split(strings.TrimPrefix(data, cbSubjectPage), ":")
	if len(parts) != 2 {
		return
	}
	subjectID, err1 := strconv.ParseInt(parts[0], 10, 64)
	page, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return
	}
	text, kb, err := h.renderSubject(ctx, user, subjectID, page, true)
	if err != nil {
		log.Printf("subject page %d/%d: %v", subjectID, page, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки предмета")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// --- Weak-topics (personal tests) ------------------------------------------------

// renderWeakMenu renders the weak-topics subject picker (main menu ->
// "🎯 Слабые темы"). Only subjects in which the user ACTUALLY has weak
// (🔴/🟡) topics are listed: a subject the user never practised cannot have
// a weak topic, and offering it led to an endless «generating…» state. No
// generation is triggered here — a test is created only on an explicit tap.
func (h *Handler) renderWeakMenu(ctx context.Context, user *models.User) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.SubjectsWithWeakTopics(ctx, user.ID)
	if err != nil {
		return "", nil, err
	}
	if len(subjects) == 0 {
		return "🎯 Слабые темы\n\nПока у тебя нет слабых тем 🙌\n\nОни появятся, когда ты пройдёшь хотя бы один обычный тест и ошибёшься в каких-то вопросах. Загляни в «📚 Предметы» — а потом возвращайся сюда за персональным тестом.",
			&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
				bot.Row(bot.Btn("📚 К предметам", cbSubjects)),
				bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
			}}, nil
	}
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbWeakSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return "🎯 Слабые темы\n\nЯ соберу персональный тест из 20 вопросов по темам, которые у тебя пока 🔴 и 🟡. Здесь только предметы, в которых у тебя есть слабые темы. Выбери предмет:", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// showWeakMenu is used from the Reply Keyboard (new message).
func (h *Handler) showWeakMenu(ctx context.Context, chatID int64, user *models.User) {
	text, kb, err := h.renderWeakMenu(ctx, user)
	if err != nil {
		log.Printf("weak menu: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки 😔", nil); err != nil {
			log.Printf("send weak menu error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		log.Printf("send weak menu: %v", err)
	}
}

// editWeakMenu is used from an inline button (edits the current message).
func (h *Handler) editWeakMenu(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	text, kb, err := h.renderWeakMenu(ctx, user)
	if err != nil {
		log.Printf("weak menu: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// openWeakSubject opens (or urgently generates) the user's PERSONAL
// weak-topics test of the subject. An unfinished attempt is resumed; a
// mastered one (15🟢+5🟡) can be finished — it is deleted and the next
// visit generates a fresh test. Nothing is generated until the user taps.
func (h *Handler) openWeakSubject(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	test, pending, topics, err := h.quiz.EnsurePersonalTest(ctx, user.ID, subjectID)
	if err != nil {
		log.Printf("personal test %d/%d: %v", user.ID, subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки теста")
		return
	}
	if len(topics) == 0 {
		// Stale button (the topics got mastered meanwhile) — refresh the
		// picker so the subject disappears from the list.
		h.answerAlert(ctx, cb, "В этом предмете у тебя больше нет слабых тем 🎉 Пройди обычные тесты в «📚 Предметы» — новые слабые темы появятся здесь.")
		h.editWeakMenu(ctx, cb, user)
		return
	}
	if test == nil {
		if !pending {
			h.answerAlert(ctx, cb, "Генерация тестов сейчас недоступна 😔 Попробуй чуть позже.")
			return
		}
		key := fmt.Sprintf("weak:%d:%d", user.ID, subjectID)
		check := func(ctx context.Context) (*models.Test, bool, error) {
			return h.quiz.PersonalTestStatus(ctx, user.ID, subjectID)
		}
		note := "⏳ Генерирую персональный тест по твоим слабым темам…\n\nНикуда не уходи, подожди минуточку — как только тест будет готов, я сразу пришлю сюда кнопку, чтобы его начать 👇"
		h.startGenerationWatch(ctx, cb, key, note, check, cbWeakSubject+strconv.FormatInt(subjectID, 10))
		return
	}
	// openTest acknowledges the tap itself (a toast when the first-ever Kazakh
	// translation of the test starts, a silent ack on the happy path) —
	// answering here as well would burn the single allowed callback answer.
	h.openTest(ctx, cb, user, test.ID)
}

// --- Test flow -----------------------------------------------------------------

// openTest opens the test immediately: tapping a test button shows the first
// (or current) question right away. If the user has an unfinished attempt it
// is resumed from where it stopped; otherwise a brand-new attempt is created.
func (h *Handler) openTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	test, err := h.quiz.GetTest(ctx, testID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Тест не найден")
		return
	} else if err != nil {
		log.Printf("get test %d: %v", testID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки теста")
		return
	}

	// A personal weak-topics test opens only for its owner.
	if test.Kind == models.TestKindPersonal && test.OwnerUserID != user.ID {
		h.answerCallback(ctx, cb, "Этот тест собран под другого ученика")
		return
	}

	// Unlock chain: a chain test opens only when the previous one is at
	// 15🟢 + 5🟡. Tapping a 🔒 button shows what is missing.
	allowed, reason, err := h.quiz.CanOpenTest(ctx, user.ID, test)
	if err != nil {
		log.Printf("can open test %d: %v", testID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки теста")
		return
	}
	if !allowed {
		// A modal alert (with OK) instead of a vanishing toast: the unlock
		// requirement is long and must be readable.
		h.answerAlert(ctx, cb, reason)
		return
	}

	// Kazakh users: make sure the test content has a Kazakh version BEFORE
	// the attempt starts. The cached translation is reused when it exists
	// (ZERO DeepSeek calls — every later Kazakh user of the same test is
	// completely free); otherwise the Russian master test is translated once
	// (one DeepSeek Flash call) and stored for everyone. A translation
	// failure never blocks the test — it opens in the Russian master version.
	// The callback is acknowledged right away (a silent ack stops the spinner);
	// every further notice goes to the chat as a normal, readable message.
	h.answerCallback(ctx, cb, "")
	cbAnswered := true
	if user.TestLang == models.TestLangKK {
		h.ensureTranslatedWithNote(ctx, cb.Message.Chat.ID, user, testID)
	}

	resume, err := h.quiz.ResumeOrNil(ctx, user.ID, testID)
	if err != nil {
		log.Printf("resume lookup %d: %v", testID, err)
		h.failOpenTest(ctx, cb, cbAnswered, "Ошибка загрузки теста")
		return
	}
	if resume != nil {
		// Continue the saved attempt from the current question — the menu
		// must be hidden for the resumed run too (the user may have left the
		// test earlier and got the menu back).
		h.ackOpenTest(ctx, cb, cbAnswered)
		h.hideReplyKeyboard(ctx, cb.Message.Chat.ID)
		h.showCurrentQuestion(ctx, cb, user, resume.ID)
		return
	}

	attempt, err := h.quiz.StartTest(ctx, user.ID, testID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.failOpenTest(ctx, cb, cbAnswered, "Тест не найден")
		return
	}
	if err != nil {
		log.Printf("start test %d: %v", testID, err)
		h.failOpenTest(ctx, cb, cbAnswered, "Не удалось начать тест")
		return
	}
	// A fresh attempt begins: hide the bottom menu for the whole run.
	h.ackOpenTest(ctx, cb, cbAnswered)
	h.hideReplyKeyboard(ctx, cb.Message.Chat.ID)
	// Show the first question of the new attempt.
	h.showCurrentQuestion(ctx, cb, user, attempt.ID)
}

// ackOpenTest acknowledges the test-button tap on the happy path (unless a
// toast was already sent): an unanswered callback leaves a spinning loader
// on the button under the user's finger.
func (h *Handler) ackOpenTest(ctx context.Context, cb *bot.CallbackQuery, already bool) {
	if already {
		return
	}
	h.answerCallback(ctx, cb, "")
}

// failOpenTest reports an open-test failure. When the translation toast was
// already sent the callback is spent (Telegram allows exactly ONE answer
// per callback_query), so the error goes as a plain chat message instead.
func (h *Handler) failOpenTest(ctx context.Context, cb *bot.CallbackQuery, cbAnswered bool, text string) {
	if !cbAnswered {
		h.answerCallback(ctx, cb, text)
		return
	}
	if _, err := h.tg.SendMessage(ctx, cb.Message.Chat.ID, text, nil); err != nil {
		log.Printf("send open-test failure: %v", err)
	}
}

// loadCurrentQuestion loads the next unanswered question view. Returns nil
// view (and nil error) when the attempt has nothing left to answer.
func (h *Handler) loadCurrentQuestion(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) *services.QuestionView {
	view, err := h.quiz.CurrentQuestion(ctx, attemptID, user)
	if err != nil {
		log.Printf("current question: %v", err)
		if cb != nil {
			h.answerCallback(ctx, cb, "Ошибка загрузки вопроса")
		}
		return nil
	}
	return view
}

// showCurrentQuestion renders the next unanswered question by EDITING the
// message that triggered the callback (used when a test is opened/resumed
// from a test button).
func (h *Handler) showCurrentQuestion(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	view := h.loadCurrentQuestion(ctx, cb, user, attemptID)
	if view == nil {
		// All questions answered -> show the result.
		h.showResult(ctx, cb, user, attemptID)
		return
	}
	h.editMessage(ctx, cb, renderQuestion(view), questionKeyboard(view))
}

// sendCurrentQuestion sends the next unanswered question as a NEW message
// (used after an answer, so the answered question above stays untouched).
func (h *Handler) sendCurrentQuestion(ctx context.Context, chatID int64, user *models.User, attemptID int64) {
	view := h.loadCurrentQuestion(ctx, nil, user, attemptID)
	if view == nil {
		// All questions answered -> show the result.
		h.showResult(ctx, nil, user, attemptID, chatID)
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, renderQuestion(view), questionKeyboard(view)); err != nil {
		log.Printf("send question: %v", err)
	}
}

func renderQuestion(v *services.QuestionView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "❓ Вопрос %d/%d\n\n%s\n\n", v.AttemptQ.Position, v.Total, v.Text)
	for i, text := range v.DisplayTexts {
		fmt.Fprintf(&b, "%s) %s\n", services.OptionLabels[i], text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func questionKeyboard(v *services.QuestionView) *bot.InlineKeyboardMarkup {
	rows := make([][]bot.InlineKeyboardButton, 0, len(v.DisplayTexts)+1)
	prefix := fmt.Sprintf("%s%d:%d:", cbAnswer, v.Attempt.ID, v.AttemptQ.Position)
	for i := range v.DisplayTexts {
		// Full-width buttons with the option text: easy to read and to tap.
		rows = append(rows, bot.Row(bot.Btn(
			fmt.Sprintf("%s) %s", services.OptionLabels[i], v.DisplayTexts[i]),
			prefix+strconv.Itoa(i),
		)))
	}
	rows = append(rows, bot.Row(bot.Btn("🚪 Выйти", cbExit+strconv.FormatInt(v.Attempt.ID, 10))))
	return &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (h *Handler) handleAnswer(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	// data: "ans:<attemptID>:<position>:<displayIndex>"
	parts := strings.Split(strings.TrimPrefix(data, cbAnswer), ":")
	if len(parts) != 3 {
		h.answerCallback(ctx, cb, "Некорректный ответ")
		return
	}
	attemptID, err1 := strconv.ParseInt(parts[0], 10, 64)
	position, err2 := strconv.Atoi(parts[1])
	displayIndex, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		h.answerCallback(ctx, cb, "Некорректный ответ")
		return
	}

	// Resolve displayed letter -> original option label using the stored shuffle.
	view, err := h.quiz.QuestionAtPosition(ctx, attemptID, user, position)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Попытка не найдена")
		return
	}
	if err != nil {
		log.Printf("load attempt question: %v", err)
		h.answerCallback(ctx, cb, "Ошибка")
		return
	}
	original, err := services.MapDisplayToOriginal(view.AttemptQ, displayIndex)
	if err != nil {
		h.answerCallback(ctx, cb, "Некорректный вариант")
		return
	}

	res, err := h.quiz.SubmitAnswer(ctx, user.ID, attemptID, position, original)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Попытка не найдена")
		return
	}
	if err != nil {
		log.Printf("submit answer: %v", err)
		h.answerCallback(ctx, cb, "Не удалось сохранить ответ")
		return
	}
	if res.AlreadyAnswered {
		// Duplicate callback — answer already counted, just acknowledge.
		h.answerCallback(ctx, cb, "Ответ уже засчитан")
		return
	}

	// Finalize the answered message: verdict + option marks + question
	// knowledge level, and REMOVE the A/B/C/D keyboard so the question
	// cannot be answered twice. The message is never edited again.
	h.editMessage(ctx, cb, renderAnswered(view, res), nil)

	chatID := cb.Message.Chat.ID
	if res.Finished {
		// Test over -> build the summary FIRST: the fresh 🟢/🟡 counts decide
		// whether the next chain test starts generating (only at 15🟢+5🟡),
		// then send the result as a new message below.
		sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
		if err != nil {
			log.Printf("summary: %v", err)
			h.answerCallback(ctx, cb, "Ошибка загрузки результата")
			return
		}
		h.quiz.OnTestCompleted(ctx, user.ID, sum.Test,
			sum.StatusCounts[models.StatusMastered], sum.StatusCounts[models.StatusPartial])
		h.renderSummary(ctx, sum, user, attemptID, chatID)
		return
	}
	// The next question is sent as a NEW message right below the answered
	// one; the answered message keeps its final state.
	h.sendCurrentQuestion(ctx, chatID, user, attemptID)
}

// renderAnswered renders the question with the verdict and the correct
// option revealed (🟢). The wrong selection is marked 🔴.
func renderAnswered(v *services.QuestionView, res *repositories.AnswerResult) string {
	var b strings.Builder
	if res.Correct {
		b.WriteString("🟢 Правильно\n\n")
	} else {
		b.WriteString("🔴 Неправильно\n\n")
	}
	fmt.Fprintf(&b, "❓ Вопрос %d/%d\n\n%s\n\n", v.AttemptQ.Position, v.Total, v.Text)
	for i, orig := range v.AttemptQ.OptionOrder {
		mark := ""
		switch {
		case orig == v.Question.CorrectAnswer:
			mark = " 🟢"
		case orig == res.SelectedAnswer:
			mark = " 🔴"
		}
		fmt.Fprintf(&b, "%s) %s%s\n", services.OptionLabels[i], v.DisplayTexts[i], mark)
	}
	if !res.Correct {
		correctText := ""
		for i, orig := range v.AttemptQ.OptionOrder {
			if orig == v.Question.CorrectAnswer {
				correctText = fmt.Sprintf("%s) %s", services.OptionLabels[i], v.DisplayTexts[i])
				break
			}
		}
		fmt.Fprintf(&b, "\n🟢 Правильный ответ: %s", correctText)
	}
	// Current knowledge level of this question after the answer.
	fmt.Fprintf(&b, "\n\nУровень вопроса: %s %d", models.StatusEmoji(res.NewStatus), res.NewStatus)
	return b.String()
}

// --- Result -----------------------------------------------------------------

// showResult renders the attempt summary as a NEW message. cb may be nil
// (called after an answer); in that case chatID must be provided.
func (h *Handler) showResult(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64, chatID ...int64) {
	sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
	if err != nil {
		log.Printf("summary: %v", err)
		if cb != nil {
			h.answerCallback(ctx, cb, "Ошибка загрузки результата")
		}
		return
	}

	// Resolve the target chat: from the callback message or the argument.
	target := int64(0)
	if cb != nil && cb.Message != nil {
		target = cb.Message.Chat.ID
	} else if len(chatID) > 0 {
		target = chatID[0]
	}
	h.renderSummary(ctx, sum, user, attemptID, target)
}

// renderSummary sends the attempt summary as a NEW message. It is separated
// from showResult so the answer flow can reuse the summary it already built
// (needed to decide whether the next test starts generating) without
// querying the database twice.
func (h *Handler) renderSummary(ctx context.Context, sum *services.AttemptSummary, user *models.User, attemptID, chatID int64) {
	target := chatID
	var b strings.Builder
	b.WriteString("🎉 Тест завершён!\n\n")
	fmt.Fprintf(&b, "🟢 Закреплено: %d\n", sum.StatusCounts[models.StatusMastered])
	fmt.Fprintf(&b, "🟡 В процессе: %d\n", sum.StatusCounts[models.StatusPartial])
	fmt.Fprintf(&b, "🔴 Требует повторения: %d\n\n", sum.StatusCounts[models.StatusNone])
	fmt.Fprintf(&b, "Результат: %d/%d", sum.Attempt.CorrectCount, sum.Total)

	green := sum.StatusCounts[models.StatusMastered]
	yellow := sum.StatusCounts[models.StatusPartial]
	idStr := strconv.FormatInt(attemptID, 10)
	rows := [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("🔄 Пройти тест ещё раз", cbRetry+idStr)),
	}
	// Personal weak-topics test: once it is mastered (15🟢 + 5🟡) it can be
	// finished — the test is deleted and the next weak-topics run generates
	// a fresh one.
	if sum.Test.Kind == models.TestKindPersonal {
		if green >= models.UnlockGreen && yellow >= models.UnlockYellow {
			b.WriteString("\n\n🏁 Ты закрыл эти слабые темы! Можешь закончить тест — в следующий раз соберу новый.")
			rows = append(rows, bot.Row(bot.Btn("🏁 Закончить тест", cbFinish+strconv.FormatInt(sum.Test.ID, 10))))
		} else {
			fmt.Fprintf(&b, "\n\nДоведи тест до %d🟢 + %d🟡 — тогда его можно будет закончить и получить новый.", models.UnlockGreen, models.UnlockYellow)
		}
		rows = append(rows, bot.Row(bot.Btn("🎯 Слабые темы", cbWeakMenu)))
	} else {
		// Chain test: reaching the bar unlocks the next test AND starts its
		// generation right away (that is the only trigger the user wants).
		// Say it here so the user knows the next test is on its way — but
		// only when a next test actually exists (the chain is capped).
		if green >= models.UnlockGreen && yellow >= models.UnlockYellow {
			if sum.Test.TestNumber+1 <= models.MaxVisibleTests {
				fmt.Fprintf(&b, "\n\n🔓 Ты открыл «Тест %d»! Я уже начал его собирать по твоим результатам — обычно это занимает пару минут ⏳", sum.Test.TestNumber+1)
			} else {
				b.WriteString("\n\n🏆 Это был последний тест цепочки — ты прошёл её целиком!")
			}
		}
		rows = append(rows, bot.Row(bot.Btn("📚 К предметам", cbSubjects)))
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
	// Send as a new message so the answered question stays visible above.
	if _, err := h.tg.SendMessage(ctx, target, b.String(), kb); err != nil {
		log.Printf("send result: %v", err)
	}
	// The run is over — the bottom menu (hidden at the start) comes back.
	h.restoreReplyKeyboard(ctx, target)
}

// finishPersonalTest handles "🏁 Закончить тест": the mastered personal
// test is deleted, so the next weak-topics visit generates a fresh one.
func (h *Handler) finishPersonalTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	if err := h.quiz.FinishPersonalTest(ctx, user.ID, testID); err != nil {
		log.Printf("finish personal test %d/%d: %v", user.ID, testID, err)
		h.answerCallback(ctx, cb, "Не удалось закончить тест")
		return
	}
	h.answerCallback(ctx, cb, "Тест завершён 🎉")
	h.editMessage(ctx, cb, "🏁 Тест по слабым темам завершён и удалён.\n\nКогда захочешь — нажми «🎯 Слабые темы» в главном меню, и я соберу новый тест по актуальным пробелам.",
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(bot.Btn("🎯 Слабые темы", cbWeakMenu)),
			bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
		}})
	// The bottom menu was already restored together with the result screen
	// — no extra note needed here.
}

func (h *Handler) retryTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
	if err != nil {
		h.answerCallback(ctx, cb, "Ошибка")
		return
	}
	// The user may have switched the test language to Kazakh since this test
	// was created — make sure the Kazakh version exists before the new run
	// (the cached translation is reused when it already exists, zero API
	// calls; otherwise the test is translated once, for everyone). A failure
	// falls back to the Russian master version, never blocks the retry.
	if user.TestLang == models.TestLangKK {
		h.ensureTranslatedWithNote(ctx, cb.Message.Chat.ID, user, sum.Test.ID)
	}
	// Every retry is a brand-new attempt with fresh shuffled question and
	// option orders.
	newAttempt, err := h.quiz.StartTest(ctx, user.ID, sum.Test.ID)
	if err != nil {
		log.Printf("retry test: %v", err)
		h.answerCallback(ctx, cb, "Не удалось начать тест")
		return
	}
	// The menu was restored with the result screen — hide it again for the
	// new run (retry bypasses openTest, which normally does this).
	h.hideReplyKeyboard(ctx, cb.Message.Chat.ID)
	h.showCurrentQuestion(ctx, cb, user, newAttempt.ID)
}

// confirmExit replaces the question with a yes/no confirmation instead of
// exiting right away — an accidental tap on 🚪 must not lose the flow.
func (h *Handler) confirmExit(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	if err := h.quiz.Exit(ctx, attemptID, user.ID); err != nil {
		log.Printf("exit confirm attempt %d: %v", attemptID, err)
		h.answerCallback(ctx, cb, "Попытка не найдена")
		return
	}
	idStr := strconv.FormatInt(attemptID, 10)
	h.editMessage(ctx, cb, "❓ Выйти из теста?\n\nПрогресс сохранится — потом можно будет продолжить с того же вопроса.",
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(
				bot.Btn("✅ Да, выйти", cbExitYes+idStr),
				bot.Btn("↩️ Нет, продолжить", cbExitNo+idStr),
			),
		}})
}

// exitTest performs the actual exit after the user confirmed it.
func (h *Handler) exitTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	if err := h.quiz.Exit(ctx, attemptID, user.ID); err != nil {
		log.Printf("exit attempt %d: %v", attemptID, err)
		h.answerCallback(ctx, cb, "Не удалось выйти")
		return
	}
	h.answerCallback(ctx, cb, "Прогресс сохранён")
	h.editMessage(ctx, cb, "🚪 Вы вышли из теста. Попытка сохранена — её можно продолжить с места остановки.",
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(bot.Btn("📚 К предметам", cbSubjects)),
			bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
		}})
	// Test is no longer running — give the bottom menu back.
	h.restoreReplyKeyboard(ctx, cb.Message.Chat.ID)
}

// cancelExit returns the user to the current question of the attempt.
func (h *Handler) cancelExit(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	h.answerCallback(ctx, cb, "Продолжаем 💪")
	h.showCurrentQuestion(ctx, cb, user, attemptID)
}

// --- Progress & settings ---------------------------------------------------------

// renderProgress renders the first progress screen: a subject picker with
// inline buttons one per row (full width — long names stay readable). The
// per-subject statistics are shown only after the user picks a subject.
func (h *Handler) renderProgress(ctx context.Context, user *models.User) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}

	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbProgSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return "📊 Мой прогресс\n\nВыберите предмет:", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// renderSubjectProgress renders the knowledge statistics of a single subject.
func (h *Handler) renderSubjectProgress(ctx context.Context, user *models.User, subjectID int64) (string, *bot.InlineKeyboardMarkup, error) {
	sp, err := h.quiz.SubjectProgress(ctx, user.ID, subjectID)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📊 Мой прогресс — %s\n\n", sp.SubjectName)
	fmt.Fprintf(&b, "🟢 Закреплено: %d\n", sp.Green)
	fmt.Fprintf(&b, "🟡 В процессе: %d\n", sp.Yellow)
	fmt.Fprintf(&b, "🔴 Требует повторения: %d\n", sp.Red)
	fmt.Fprintf(&b, "Всего вопросов: %d\n", sp.TotalQuestions)
	// The personal weak-topics test (and its questions) is training material,
	// not part of the course: its questions are excluded from the totals so a
	// 20-question personal test never inflates the counters (the 🔴 count
	// used to jump by the size of the personal test).
	b.WriteString("\n🎯 Персональный тест по слабым темам в общую статистику не входит.")

	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("📚 К тестам предмета", cbSubject+strconv.FormatInt(subjectID, 10))),
		bot.Row(bot.Btn("⬅️ К выбору предмета", cbProgress)),
	}}
	return b.String(), kb, nil
}

// showSubjectProgress shows statistics of one subject (from the picker).
func (h *Handler) showSubjectProgress(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	text, kb, err := h.renderSubjectProgress(ctx, user, subjectID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Предмет не найден")
		return
	}
	if err != nil {
		log.Printf("subject progress %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки прогресса")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// showProgress is used from the Reply Keyboard (new message).
func (h *Handler) showProgress(ctx context.Context, chatID int64, user *models.User) {
	text, kb, err := h.renderProgress(ctx, user)
	if err != nil {
		log.Printf("user progress: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки прогресса 😔", nil); err != nil {
			log.Printf("send progress error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		log.Printf("send progress: %v", err)
	}
}

// editProgress is used from an inline button (edits the current message).
func (h *Handler) editProgress(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	text, kb, err := h.renderProgress(ctx, user)
	if err != nil {
		log.Printf("user progress: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки прогресса")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// settingsKeyboard renders the language-of-tests picker: the current choice
// is marked with ✅. The interface itself always stays Russian — this
// setting changes ONLY the language of the test content.
func settingsKeyboard(lang string) *bot.InlineKeyboardMarkup {
	ru, kk := "🇷🇺 Русский", "🇰🇿 Қазақша"
	if lang == models.TestLangKK {
		kk = "✅ " + kk
	} else {
		ru = "✅ " + ru
	}
	return &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn(ru, cbSetLang+models.TestLangRU)),
		bot.Row(bot.Btn(kk, cbSetLang+models.TestLangKK)),
		bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
	}}
}

const settingsText = "⚙️ Настройки\n\n🌐 Язык тестов\n\nВыберите язык, на котором показываются вопросы и варианты ответов. Интерфейс бота остаётся на русском.\n\nЕсли казахской версии теста ещё нет, я переведу его один раз и сохраню — дальше она откроется мгновенно."

// showSettings is used from the Reply Keyboard (new message).
func (h *Handler) showSettings(ctx context.Context, chatID int64, user *models.User) {
	if _, err := h.tg.SendMessage(ctx, chatID, settingsText, settingsKeyboard(user.TestLang)); err != nil {
		log.Printf("send settings: %v", err)
	}
}

// editSettings is used from an inline button (edits the current message).
func (h *Handler) editSettings(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	h.editMessage(ctx, cb, settingsText, settingsKeyboard(user.TestLang))
}

// setTestLang switches the language of the test content (🇷🇺/🇰🇿) and
// re-renders the settings screen with the new checkmark.
func (h *Handler) setTestLang(ctx context.Context, cb *bot.CallbackQuery, user *models.User, lang string) {
	if err := h.users.SetTestLang(ctx, user.ID, lang); err != nil {
		log.Printf("set test lang %d -> %q: %v", user.ID, lang, err)
		h.answerCallback(ctx, cb, "Не удалось сохранить настройку")
		return
	}
	user.TestLang = lang
	if lang == models.TestLangKK {
		h.answerCallback(ctx, cb, "Язык тестов: Қазақша 🇰🇿")
	} else {
		h.answerCallback(ctx, cb, "Язык тестов: Русский 🇷🇺")
	}
	h.editMessage(ctx, cb, settingsText, settingsKeyboard(lang))
}

// --- Leaderboard ----------------------------------------------------------------

// renderLeaderboardMenu renders the leaderboard picker: one button per
// subject (levels + 🟢 board of that subject) plus the 🔥 streak board.
func (h *Handler) renderLeaderboardMenu(ctx context.Context) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects)+1)
	buttons = append(buttons, bot.Btn("🔥 Огоньки (дни подряд)", cbLbStreak))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbLbSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return "🏆 Таблица лидеров\n\nВыбери, что смотрим:\n• 🔥 Огоньки — кто сколько дней подряд занимается;\n• Предмет — кто сколько уровней открыл и у кого больше всего 🟢.", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (h *Handler) showLeaderboardMenu(ctx context.Context, chatID int64) {
	text, kb, err := h.renderLeaderboardMenu(ctx)
	if err != nil {
		log.Printf("leaderboard menu: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки 😔", nil); err != nil {
			log.Printf("send leaderboard menu error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		log.Printf("send leaderboard menu: %v", err)
	}
}

func (h *Handler) editLeaderboardMenu(ctx context.Context, cb *bot.CallbackQuery) {
	text, kb, err := h.renderLeaderboardMenu(ctx)
	if err != nil {
		log.Printf("leaderboard menu: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// lbName renders a leaderboard row name: first name, falling back to the
// @username, then to a neutral placeholder.
func lbName(e *models.LeaderboardEntry) string {
	if n := strings.TrimSpace(e.FirstName); n != "" {
		return n
	}
	if n := strings.TrimSpace(e.Username); n != "" {
		return "@" + n
	}
	return "Участник"
}

// flame renders the streak suffix of a leaderboard row (" 🔥5"), empty when
// the streak has not lit up yet (day one).
func flame(e *models.LeaderboardEntry) string {
	if e.StreakDays >= 2 {
		return fmt.Sprintf(" 🔥%d", e.StreakDays)
	}
	return ""
}

// showStreakLeaderboard renders the 🔥 board: longest daily streaks first.
func (h *Handler) showStreakLeaderboard(ctx context.Context, cb *bot.CallbackQuery) {
	entries, err := h.quiz.StreakLeaderboard(ctx, services.LeaderboardSize)
	if err != nil {
		log.Printf("streak leaderboard: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	var b strings.Builder
	b.WriteString("🔥 Топ по огонькам\n\n")
	if len(entries) == 0 {
		b.WriteString("Пока пусто — заходи каждый день, и твой огонёк появится здесь!")
	} else {
		b.WriteString("Дней подряд занимаются:\n\n")
		for i := range entries {
			e := &entries[i]
			mark := "—"
			if e.StreakDays >= 2 {
				mark = fmt.Sprintf("🔥 %d %s", e.StreakDays, dayWord(e.StreakDays))
			}
			fmt.Fprintf(&b, "%d. %s — %s\n", i+1, lbName(e), mark)
		}
		b.WriteString("\nОгонёк растёт каждый день, когда ты заходишь, и гаснет, если пропустить день.")
	}
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("⬅️ К таблице лидеров", cbLeaderboard)),
	}}
	h.editMessage(ctx, cb, b.String(), kb)
}

// showSubjectLeaderboard renders the board of one subject: unlocked levels
// and total 🟢 per user.
func (h *Handler) showSubjectLeaderboard(ctx context.Context, cb *bot.CallbackQuery, subjectID int64) {
	subject, err := h.quiz.GetSubject(ctx, subjectID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Предмет не найден")
		return
	}
	if err != nil {
		log.Printf("leaderboard subject %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	byLevels, err := h.quiz.UnlockedTestsLeaderboard(ctx, subjectID, services.LeaderboardSize)
	if err != nil {
		log.Printf("levels leaderboard %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	byGreen, err := h.quiz.GreenLeaderboard(ctx, subjectID, services.LeaderboardSize)
	if err != nil {
		log.Printf("green leaderboard %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🏆 %s %s\n\n", subjectEmoji(subject.Name), subject.Name)
	b.WriteString("По открытым уровням:\n")
	if len(byLevels) == 0 {
		b.WriteString("Пока никто не начал — будь первым!\n")
	} else {
		for i := range byLevels {
			e := &byLevels[i]
			fmt.Fprintf(&b, "%d. %s — %d ур.%s\n", i+1, lbName(e), e.UnlockedTests, flame(e))
		}
	}
	b.WriteString("\nПо закреплённым вопросам 🟢:\n")
	if len(byGreen) == 0 {
		b.WriteString("Пока никого нет — пройди тест и стань первым!\n")
	} else {
		for i := range byGreen {
			e := &byGreen[i]
			fmt.Fprintf(&b, "%d. %s — %d🟢%s\n", i+1, lbName(e), e.Green, flame(e))
		}
	}
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("⬅️ К таблице лидеров", cbLeaderboard)),
	}}
	h.editMessage(ctx, cb, b.String(), kb)
}

// --- Callbacks router -----------------------------------------------------------

func (h *Handler) handleCallback(ctx context.Context, cb *bot.CallbackQuery) {
	user, err := h.ensureUser(ctx, cb.From)
	if err != nil {
		log.Printf("upsert user %d: %v", cb.From.ID, err)
		return
	}
	data := cb.Data

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
	case data == cbPending:
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Подожди минуточку и открой предмет снова.")
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

// --- Helpers -------------------------------------------------------------------

func (h *Handler) editMessage(ctx context.Context, cb *bot.CallbackQuery, text string, kb *bot.InlineKeyboardMarkup) {
	if cb.Message == nil {
		return
	}
	if err := h.tg.EditMessageText(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, kb); err != nil {
		// "message is not modified" means the content on screen is ALREADY
		// exactly this — sending a fresh copy would duplicate messages in the
		// chat (it happened on repeated taps). Everything else (message too
		// old to edit, deleted message) falls back to a new message.
		if strings.Contains(err.Error(), "message is not modified") {
			return
		}
		if _, err2 := h.tg.SendMessage(ctx, cb.Message.Chat.ID, text, kb); err2 != nil {
			log.Printf("edit/send message: %v / %v", err, err2)
		}
	}
}

// answerAlert answers the callback with a modal alert (dialog with an OK
// button) — for notices that must actually be read, unlike the tiny toast.
func (h *Handler) answerAlert(ctx context.Context, cb *bot.CallbackQuery, text string) {
	if err := h.tg.AnswerCallbackAlert(ctx, cb.ID, text); err != nil {
		log.Printf("answer callback alert: %v", err)
	}
}

func (h *Handler) answerCallback(ctx context.Context, cb *bot.CallbackQuery, text string) {
	if err := h.tg.AnswerCallbackQuery(ctx, cb.ID, text); err != nil {
		log.Printf("answer callback: %v", err)
	}
}

// --- «Please wait» notices ---------------------------------------------------

const (
	// genWatchInterval is how often the watcher checks whether the awaited
	// test has been generated.
	genWatchInterval = 5 * time.Second
	// genWatchTimeout caps the wait (a real generation takes ~1–3 minutes,
	// the worker's per-job timeout is 8 minutes).
	genWatchTimeout = 10 * time.Minute
)

// ensureTranslatedWithNote makes sure the Kazakh version of the test exists.
// When the first-ever translation is about to run (it takes up to a
// minute), the user gets a clear CHAT MESSAGE instead of an unreadable
// 5-second toast; the note is removed as soon as the test is ready.
func (h *Handler) ensureTranslatedWithNote(ctx context.Context, chatID int64, user *models.User, testID int64) {
	done, err := h.quiz.TestFullyTranslated(ctx, testID)
	if err != nil {
		log.Printf("translation status %d: %v", testID, err)
		done = true // unknown — do not promise a wait we cannot judge
	}
	var noteID int64
	if !done {
		id, serr := h.tg.SendMessage(ctx, chatID,
			"⏳ Тест переводится на казахский язык 🇰🇿\n\nНикуда не уходи, подожди минуточку — перевод делается один раз, дальше этот тест будет открываться мгновенно. Первый вопрос появится сам 👆", nil)
		if serr != nil {
			log.Printf("send translation note: %v", serr)
		} else {
			noteID = id
		}
	}
	if _, terr := h.quiz.EnsureTestTranslated(ctx, user, testID); terr != nil {
		// A translation failure never blocks the test: the Russian master
		// version is served instead.
		log.Printf("translate test %d: %v", testID, terr)
		if noteID != 0 {
			if eerr := h.tg.EditMessageText(ctx, chatID, noteID,
				"😔 Не получилось перевести тест на казахский — пока открываю его на русском. Попробуй позже ещё раз.", nil); eerr != nil {
				log.Printf("edit translation note: %v", eerr)
			}
		}
		return
	}
	if noteID != 0 {
		if derr := h.tg.DeleteMessage(ctx, chatID, noteID); derr != nil {
			log.Printf("delete translation note: %v", derr)
		}
	}
}

// startGenerationWatch acknowledges the tap, posts a clearly visible chat
// message «⏳ тест генерируется, никуда не уходи…» and starts a background
// watcher that edits that very message into «✅ Тест готов» with a start
// button once the test exists — or into an honest error with a retry button
// when the generation failed / takes too long. One watcher per key: repeated
// taps never spam the chat.
func (h *Handler) startGenerationWatch(ctx context.Context, cb *bot.CallbackQuery, key, note string,
	check func(context.Context) (*models.Test, bool, error), retryData string) {
	if _, busy := h.watchers.LoadOrStore(key, struct{}{}); busy {
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Никуда не уходи — как только он будет готов, я пришлю в чат кнопку, чтобы его начать.")
		return
	}
	h.answerCallback(ctx, cb, "")
	chatID := cb.Message.Chat.ID
	msgID, err := h.tg.SendMessage(ctx, chatID, note, nil)
	if err != nil {
		log.Printf("send generation note: %v", err)
		h.watchers.Delete(key)
		return
	}
	go h.watchGeneration(key, chatID, msgID, check, retryData)
}

// watchGeneration polls until the awaited test appears, then edits the
// «please wait» note into a ready/failed/timeout message.
func (h *Handler) watchGeneration(key string, chatID, msgID int64,
	check func(context.Context) (*models.Test, bool, error), retryData string) {
	defer h.watchers.Delete(key)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("generation watcher %s panicked: %v", key, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), genWatchTimeout+30*time.Second)
	defer cancel()

	retryKb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("🔄 Попробовать ещё раз", retryData)),
		bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
	}}
	deadline := time.Now().Add(genWatchTimeout)
	misses := 0 // consecutive checks with neither a test nor an active job
	ticker := time.NewTicker(genWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		test, pending, err := check(ctx)
		if err != nil {
			log.Printf("generation watcher %s: %v", key, err)
		} else if test != nil {
			ready := fmt.Sprintf("✅ Готово! «Тест %d» сгенерирован.", test.TestNumber)
			if test.Kind == models.TestKindPersonal {
				ready = "✅ Готово! Персональный тест по твоим слабым темам собран."
			}
			h.editNote(ctx, chatID, msgID, ready+"\n\nНажми кнопку ниже, чтобы начать 👇",
				&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
					bot.Row(bot.Btn("▶️ Начать тест", cbOpenTest+strconv.FormatInt(test.ID, 10))),
				}})
			return
		} else if !pending {
			// No test and no active job: the generation failed for good. Two
			// consecutive misses guard against the short window between the
			// job being marked done and the test becoming visible.
			misses++
			if misses >= 2 {
				h.editNote(ctx, chatID, msgID, "😔 Не получилось сгенерировать тест. Нажми «🔄 Попробовать ещё раз» — я перезапущу генерацию.", retryKb)
				return
			}
		} else {
			misses = 0
		}
		if time.Now().After(deadline) {
			h.editNote(ctx, chatID, msgID, "⏳ Генерация идёт дольше обычного. Нажми «🔄 Попробовать ещё раз» чуть позже — если тест уже готов, он сразу откроется.", retryKb)
			return
		}
	}
}

// editNote edits a bot-sent note; when editing fails (e.g. the user deleted
// the message) the text is sent as a new message instead.
func (h *Handler) editNote(ctx context.Context, chatID, msgID int64, text string, kb *bot.InlineKeyboardMarkup) {
	if err := h.tg.EditMessageText(ctx, chatID, msgID, text, kb); err != nil {
		if strings.Contains(err.Error(), "message is not modified") {
			return
		}
		if _, err2 := h.tg.SendMessage(ctx, chatID, text, kb); err2 != nil {
			log.Printf("edit/send note: %v / %v", err, err2)
		}
	}
}

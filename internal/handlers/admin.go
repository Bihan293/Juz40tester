package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// --- Admin panel (docs/ADMIN.md) ----------------------------------------------------
//
// Administrators are the Telegram ids of ADMIN_IDS. Every admin callback
// and message is re-checked against that list on the server: a non-admin
// tapping a forged «adm:…» button gets a silent acknowledgement and
// nothing else, /admin is answered like an unknown command (main menu), so
// the panel is invisible to everybody else.

// kbAdmin is the reply-keyboard button of the admin panel (admins only).
const kbAdmin = "🛠 Админка"

// Admin callbacks (prefix "adm:").
const (
	cbAdmPrefix    = "adm:"
	cbAdmMenu      = "adm:menu"
	cbAdmStats     = "adm:stats"
	cbAdmUsers     = "adm:users"
	cbAdmUser      = "adm:u:"    // + userID — user card
	cbAdmPlan      = "adm:plan:" // + userID — plan picker
	cbAdmPlanDays  = "adm:pd:"   // + userID:plan — term picker
	cbAdmPlanAsk   = "adm:pc:"   // + userID:plan:days — confirmation
	cbAdmPlanApply = "adm:py:"   // + userID:plan:days — apply
	cbAdmBc        = "adm:bc"    // broadcast menu
	cbAdmBcNew     = "adm:bcnew"
	cbAdmBcNoMedia = "adm:bcnomedia"
	cbAdmBcNoBtn   = "adm:bcnobtn"
	cbAdmBcSend    = "adm:bcsend:" // + broadcastID — confirm sending
	cbAdmBcList    = "adm:bclist"
	cbAdmBcCard    = "adm:b:"    // + broadcastID
	cbAdmBcCancel  = "adm:bcx:"  // + broadcastID — ask
	cbAdmBcCancelY = "adm:bcxy:" // + broadcastID — confirmed
	cbAdmAbort     = "adm:abort" // leave the input mode
)

// Admin dialog states (admin_sessions.state).
const (
	admStateSearch    = "search"
	admStateBcText    = "bc_text"
	admStateBcMedia   = "bc_media"
	admStateBcButtons = "bc_buttons"
	admStateBcPreview = "bc_preview"
)

// admSessionTTL: a dialog step older than this is forgotten (the admin's
// next message is handled normally again).
const admSessionTTL = 30 * time.Minute

// admPlanTerms are the offered terms of a paid plan (days).
var admPlanTerms = []int{7, 30, 90, 365}

// BroadcastWaker wakes the broadcast sender (nil = rely on its poll).
type BroadcastWaker interface{ Wake() }

// WithAdminPanel turns the admin panel on (nil = only the old /grant …
// commands).
func (h *Handler) WithAdminPanel(svc *services.AdminService, waker BroadcastWaker) *Handler {
	h.admin = svc
	h.bcWaker = waker
	return h
}

// isAdminTG is THE administrator check of the handlers (ADMIN_IDS).
func (h *Handler) isAdminTG(tgUserID int64) bool {
	return h.isAdmin != nil && h.isAdmin(tgUserID)
}

// adminPanel reports whether the admin panel is available to this user.
func (h *Handler) adminPanel(tgUserID int64) bool {
	return h.admin != nil && h.isAdminTG(tgUserID) && h.admin.IsAdmin(tgUserID)
}

// menuKeyboard is the main reply keyboard of this chat (private chat:
// chat id = Telegram user id) — «🛠 Админка» only for administrators.
func (h *Handler) menuKeyboard(chatID int64) *bot.ReplyKeyboardMarkup {
	kb := mainMenuKeyboardFor(h.billing != nil)
	if h.adminPanel(chatID) {
		kb.Keyboard = append(kb.Keyboard, bot.ReplyRow(kbAdmin))
	}
	return kb
}

// --- Messages -------------------------------------------------------------------------

// handleAdminMessage handles a message of an administrator that belongs to
// the admin panel (/admin, «🛠 Админка», the input of a dialog step).
// false = not an admin message — handled as usual.
func (h *Handler) handleAdminMessage(ctx context.Context, m *bot.Message, user *models.User) bool {
	if !h.adminPanel(m.From.ID) {
		return false
	}
	text := strings.TrimSpace(m.Text)
	if key := commandKey(text); key == "/admin" || text == kbAdmin {
		h.clearAdminSession(ctx, m.From.ID)
		h.sendAdminMenu(ctx, m.Chat.ID)
		return true
	}
	sess, err := h.admin.Repo().GetSession(ctx, m.From.ID)
	if err != nil {
		logf("admin session %d: %v", m.From.ID, err)
		return false
	}
	if sess.State == "" || time.Since(sess.UpdatedAt) > admSessionTTL {
		return false
	}
	// A command or a main-menu button leaves the dialog.
	if strings.HasPrefix(text, "/") || isMenuButton(text) {
		h.clearAdminSession(ctx, m.From.ID)
		return false
	}
	switch sess.State {
	case admStateSearch:
		h.adminSearch(ctx, m.Chat.ID, text)
	case admStateBcText:
		h.bcTakeText(ctx, m, sess)
	case admStateBcMedia:
		h.bcTakeMedia(ctx, m, sess)
	case admStateBcButtons:
		h.bcTakeButtons(ctx, m, sess)
	case admStateBcPreview:
		h.sendWithKeyboard(ctx, m.Chat.ID, "Рассылка ждёт подтверждения — нажми кнопку под предпросмотром или начни заново.",
			bot.Row(bot.Btn("✏️ Начать заново", cbAdmBcNew)), bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
	default:
		return false
	}
	return true
}

func isMenuButton(text string) bool {
	switch text {
	case kbSubjects, kbWeak, kbCustom, kbProgress, kbTop, kbSettings, kbPlans, kbAdmin:
		return true
	}
	return false
}

func (h *Handler) setAdminSession(ctx context.Context, tgID int64, state string, data map[string]any) bool {
	if err := h.admin.Repo().SetSession(ctx, tgID, state, data); err != nil {
		logf("admin session save %d: %v", tgID, err)
		h.sendText(ctx, tgID, "Ошибка сохранения состояния 😔 Попробуй ещё раз.")
		return false
	}
	return true
}

func (h *Handler) clearAdminSession(ctx context.Context, tgID int64) {
	if err := h.admin.Repo().ClearSession(ctx, tgID); err != nil {
		logf("admin session clear %d: %v", tgID, err)
	}
}

// --- Callbacks ------------------------------------------------------------------------

// handleAdminCallback routes «adm:…» callbacks. Non-admins get a silent
// acknowledgement and nothing else.
func (h *Handler) handleAdminCallback(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	if !h.adminPanel(cb.From.ID) {
		h.answerCallback(ctx, cb, "")
		return
	}
	chatID := cb.Message.Chat.ID
	switch {
	case data == cbAdmMenu:
		h.answerCallback(ctx, cb, "")
		h.clearAdminSession(ctx, cb.From.ID)
		text, kb := adminMenu()
		h.editMessage(ctx, cb, text, kb)
	case data == cbAdmAbort:
		h.answerCallback(ctx, cb, "Отменено")
		h.clearAdminSession(ctx, cb.From.ID)
		text, kb := adminMenu()
		h.editMessage(ctx, cb, text, kb)
	case data == cbAdmStats:
		h.adminStats(ctx, cb)
	case data == cbAdmUsers:
		h.answerCallback(ctx, cb, "")
		if h.setAdminSession(ctx, cb.From.ID, admStateSearch, nil) {
			h.editMessage(ctx, cb, "👥 Пользователи\n\nПришли @username, Telegram ID (или внутренний id) либо часть имени — я найду пользователя.",
				&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{bot.Row(bot.Btn("⬅️ Админка", cbAdmMenu))}})
		}
	case strings.HasPrefix(data, cbAdmUser):
		h.answerCallback(ctx, cb, "")
		if id, err := strconv.ParseInt(strings.TrimPrefix(data, cbAdmUser), 10, 64); err == nil {
			h.showUserCard(ctx, cb, chatID, id)
		}
	case strings.HasPrefix(data, cbAdmPlan):
		h.answerCallback(ctx, cb, "")
		if id, err := strconv.ParseInt(strings.TrimPrefix(data, cbAdmPlan), 10, 64); err == nil {
			h.showPlanPicker(ctx, cb, id)
		}
	case strings.HasPrefix(data, cbAdmPlanDays):
		h.answerCallback(ctx, cb, "")
		h.showPlanTerms(ctx, cb, strings.TrimPrefix(data, cbAdmPlanDays))
	case strings.HasPrefix(data, cbAdmPlanAsk):
		h.answerCallback(ctx, cb, "")
		h.askPlanChange(ctx, cb, strings.TrimPrefix(data, cbAdmPlanAsk))
	case strings.HasPrefix(data, cbAdmPlanApply):
		h.applyPlanChange(ctx, cb, strings.TrimPrefix(data, cbAdmPlanApply))
	case data == cbAdmBc:
		h.answerCallback(ctx, cb, "")
		h.clearAdminSession(ctx, cb.From.ID)
		h.editMessage(ctx, cb, "📣 Рассылка\n\nСообщение получат все пользователи бота, кроме заблокировавших его. Отправка идёт в фоне с учётом лимитов Telegram; прогресс сохраняется, после перезапуска рассылка продолжится без повторов.",
			&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
				bot.Row(bot.Btn("➕ Новая рассылка", cbAdmBcNew)),
				bot.Row(bot.Btn("📜 Прошлые рассылки", cbAdmBcList)),
				bot.Row(bot.Btn("⬅️ Админка", cbAdmMenu)),
			}})
	case data == cbAdmBcNew:
		h.answerCallback(ctx, cb, "")
		if h.setAdminSession(ctx, cb.From.ID, admStateBcText, nil) {
			h.sendWithKeyboard(ctx, chatID, "📝 Шаг 1/3. Пришли текст рассылки одним сообщением.\n\nФорматирование (жирный, ссылки и т. п.) сохранится. Можно сразу прислать фото или видео с подписью — тогда шаг с медиа пропускается.",
				bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
		}
	case data == cbAdmBcNoMedia:
		h.answerCallback(ctx, cb, "")
		h.bcSkipMedia(ctx, cb.From.ID, chatID)
	case data == cbAdmBcNoBtn:
		h.answerCallback(ctx, cb, "")
		h.bcPreview(ctx, cb.From.ID, chatID, nil)
	case strings.HasPrefix(data, cbAdmBcSend):
		if id, err := strconv.ParseInt(strings.TrimPrefix(data, cbAdmBcSend), 10, 64); err == nil {
			h.bcConfirm(ctx, cb, id)
		} else {
			h.answerCallback(ctx, cb, "")
		}
	case data == cbAdmBcList:
		h.answerCallback(ctx, cb, "")
		h.bcList(ctx, cb)
	case strings.HasPrefix(data, cbAdmBcCard):
		h.answerCallback(ctx, cb, "")
		if id, err := strconv.ParseInt(strings.TrimPrefix(data, cbAdmBcCard), 10, 64); err == nil {
			h.bcCard(ctx, cb, chatID, id)
		}
	case strings.HasPrefix(data, cbAdmBcCancelY):
		if id, err := strconv.ParseInt(strings.TrimPrefix(data, cbAdmBcCancelY), 10, 64); err == nil {
			h.bcCancel(ctx, cb, id)
		} else {
			h.answerCallback(ctx, cb, "")
		}
	case strings.HasPrefix(data, cbAdmBcCancel):
		h.answerCallback(ctx, cb, "")
		if id, err := strconv.ParseInt(strings.TrimPrefix(data, cbAdmBcCancel), 10, 64); err == nil {
			idS := strconv.FormatInt(id, 10)
			h.editMessage(ctx, cb, fmt.Sprintf("Остановить рассылку #%d? Неотправленные сообщения отправлены не будут.", id),
				&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
					bot.Row(bot.Btn("⛔ Да, остановить", cbAdmBcCancelY+idS), bot.Btn("↩️ Нет", cbAdmBcCard+idS)),
				}})
		}
	default:
		h.answerCallback(ctx, cb, "")
	}
}

// --- Menu & statistics ---------------------------------------------------------------

func adminMenu() (string, *bot.InlineKeyboardMarkup) {
	return "🛠 Админ-панель JUZ40\n\nВыбери раздел:", &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("📊 Статистика", cbAdmStats)),
		bot.Row(bot.Btn("👥 Пользователи", cbAdmUsers)),
		bot.Row(bot.Btn("📣 Рассылка", cbAdmBc)),
		bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
	}}
}

func (h *Handler) sendAdminMenu(ctx context.Context, chatID int64) {
	text, kb := adminMenu()
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send admin menu: %v", err)
	}
}

func (h *Handler) adminStats(ctx context.Context, cb *bot.CallbackQuery) {
	st, err := h.admin.Stats(ctx)
	if err != nil {
		logf("admin stats: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки статистики")
		return
	}
	h.answerCallback(ctx, cb, "")
	h.editMessage(ctx, cb, h.renderAdminStats(st), &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("🔄 Обновить", cbAdmStats)),
		bot.Row(bot.Btn("⬅️ Админка", cbAdmMenu)),
	}})
}

func (h *Handler) renderAdminStats(st *repositories.AdminStats) string {
	var b strings.Builder
	now := time.Now().In(h.admin.Location())
	fmt.Fprintf(&b, "📊 Статистика бота (на %s)\n\n", now.Format("02.01.2006 15:04"))
	fmt.Fprintf(&b, "👥 Пользователи: %d\n", st.UsersTotal)
	fmt.Fprintf(&b, "• новые: сегодня %d · 7 дней %d · 30 дней %d\n", st.NewToday, st.New7d, st.New30d)
	fmt.Fprintf(&b, "• активные: сегодня %d · 7 дней %d · 30 дней %d\n", st.ActiveToday, st.Active7d, st.Active30d)
	fmt.Fprintf(&b, "• заблокировали бота: %d\n\n", st.Blocked)
	fmt.Fprintf(&b, "📝 Тесты\n• начато сегодня: %d\n• пройдено: сегодня %d · 7 дней %d · всего %d\n• сейчас в процессе: %d\n\n",
		st.StartedToday, st.CompletedToday, st.Completed7d, st.CompletedTotal, st.InProgress)
	b.WriteString("⭐ Подписки\n")
	if h.billing == nil {
		b.WriteString("• выключены (SUBSCRIPTIONS_ENABLED=0)\n")
	} else {
		paid := 0
		for _, p := range h.billing.Catalog().Plans() {
			if p.IsFree() {
				continue
			}
			n := st.PaidPlans[p.Code]
			paid += n
			fmt.Fprintf(&b, "• %s: %d\n", p.Title, n)
		}
		fmt.Fprintf(&b, "• Free: %d\n", max(st.UsersTotal-paid, 0))
	}
	fmt.Fprintf(&b, "\n💰 Платежи (реальные Stars)\n• всего: %d на %d⭐\n• 30 дней: %d на %d⭐\n• сегодня: %d на %d⭐\n• возвращено: %d · ждут возврата: %d\n• тестовых покупок админов: %d\n",
		st.Payments, st.Stars, st.Payments30d, st.Stars30d, st.PaymentsToday, st.StarsToday, st.Refunded, st.RefundPending, st.Bypass)
	if st.WeakOrdersInFlight > 0 {
		fmt.Fprintf(&b, "• оплаченных тестов по слабым темам в сборке: %d\n", st.WeakOrdersInFlight)
	}
	b.WriteString("\n⚙️ Очереди\n")
	if len(st.GenJobs) == 0 {
		b.WriteString("• генерация: пусто\n")
	} else {
		keys := make([]string, 0, len(st.GenJobs))
		for k := range st.GenJobs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("• генерация:")
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%d", k, st.GenJobs[k])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "• ошибок генерации за сутки: %d\n", st.GenFailed24h)
	fmt.Fprintf(&b, "• переводы: в очереди %d · в работе %d\n", st.TrPending, st.TrRunning)
	fmt.Fprintf(&b, "• апдейты Telegram (очередь кластера): в работе %d · dead %d\n", st.UpdatesPending, st.UpdatesDead)
	fmt.Fprintf(&b, "• активных рассылок: %d", st.BroadcastsActive)
	return b.String()
}

// --- Users ----------------------------------------------------------------------------

func (h *Handler) adminSearch(ctx context.Context, chatID int64, q string) {
	if q == "" {
		h.sendText(ctx, chatID, "Пришли @username, ID или имя текстом.")
		return
	}
	found, err := h.admin.Search(ctx, q)
	if err != nil {
		logf("admin search %q: %v", q, err)
		h.sendText(ctx, chatID, "Ошибка поиска 😔")
		return
	}
	back := bot.Row(bot.Btn("⬅️ Админка", cbAdmMenu))
	switch len(found) {
	case 0:
		h.sendWithKeyboard(ctx, chatID, fmt.Sprintf("Никого не нашёл по запросу «%s». Пришли другой @username, ID или имя.", q), back)
	case 1:
		h.clearAdminSession(ctx, chatID)
		h.showUserCard(ctx, nil, chatID, found[0].ID)
	default:
		rows := make([][]bot.InlineKeyboardButton, 0, len(found)+1)
		for _, u := range found {
			label := u.DisplayName()
			if u.Username != "" && !strings.HasPrefix(label, "@") {
				label += " @" + u.Username
			}
			rows = append(rows, bot.Row(bot.Btn(fmt.Sprintf("%s · %d", label, u.TelegramID), cbAdmUser+strconv.FormatInt(u.ID, 10))))
		}
		rows = append(rows, back)
		if _, err := h.tg.SendMessage(ctx, chatID, fmt.Sprintf("Нашёл %d — выбери пользователя (или пришли запрос точнее):", len(found)),
			&bot.InlineKeyboardMarkup{InlineKeyboard: rows}); err != nil {
			logf("send search results: %v", err)
		}
	}
}

// showUserCard renders the user card (edits the message of cb, or sends a
// new one when cb is nil).
func (h *Handler) showUserCard(ctx context.Context, cb *bot.CallbackQuery, chatID, userID int64) {
	c, err := h.admin.Card(ctx, userID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.sendText(ctx, chatID, "Пользователь не найден")
		return
	}
	if err != nil {
		logf("admin card %d: %v", userID, err)
		h.sendText(ctx, chatID, "Ошибка загрузки карточки 😔")
		return
	}
	text := h.renderUserCard(c)
	idS := strconv.FormatInt(userID, 10)
	rows := [][]bot.InlineKeyboardButton{}
	if h.billing != nil {
		rows = append(rows, bot.Row(bot.Btn("⭐ Изменить подписку", cbAdmPlan+idS)))
	}
	rows = append(rows,
		bot.Row(bot.Btn("🔄 Обновить", cbAdmUser+idS), bot.Btn("🔎 Другой пользователь", cbAdmUsers)),
		bot.Row(bot.Btn("⬅️ Админка", cbAdmMenu)))
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
	if cb != nil {
		h.editMessage(ctx, cb, text, kb)
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send user card: %v", err)
	}
}

func fmtDate(t *time.Time, loc *time.Location, layout string) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.In(loc).Format(layout)
}

func (h *Handler) renderUserCard(c *services.UserCard) string {
	loc := h.admin.Location()
	u := c.User
	var b strings.Builder
	fmt.Fprintf(&b, "👤 %s\n\n", u.DisplayName())
	fmt.Fprintf(&b, "ID: %d (Telegram) · %d (в базе)\n", u.TelegramID, u.ID)
	if u.Username != "" {
		fmt.Fprintf(&b, "Ник: @%s\n", u.Username)
	} else {
		b.WriteString("Ник: —\n")
	}
	fmt.Fprintf(&b, "Имя: %s\n", strings.TrimSpace(u.FirstName+" "+u.LastName))
	fmt.Fprintf(&b, "Регистрация: %s\n", u.CreatedAt.In(loc).Format("02.01.2006 15:04"))
	last := "—"
	if u.LastActiveDate != nil {
		last = u.LastActiveDate.Format("02.01.2006")
	}
	fmt.Fprintf(&b, "Последняя активность: %s · 🔥 серия %d\n", last, models.EffectiveStreak(u.StreakDays, derefTime(u.LastActiveDate), models.StreakToday(time.Now())))
	lang := "русский"
	if u.TestLang == models.TestLangKK {
		lang = "казахский"
	}
	fmt.Fprintf(&b, "Язык тестов: %s\n", lang)
	if u.BlockedAt != nil {
		fmt.Fprintf(&b, "⛔ Заблокировал бота: %s\n", u.BlockedAt.In(loc).Format("02.01.2006 15:04"))
	}
	if a := c.Activity; a != nil {
		fmt.Fprintf(&b, "\n📝 Прохождения: сегодня %d · всего %d (попыток %d, в процессе %d)\n", a.CompletedToday, a.CompletedTotal, a.AttemptsTotal, a.InProgress)
		fmt.Fprintf(&b, "Последний старт теста: %s\n", fmtDate(a.LastAttemptAt, loc, "02.01.2006 15:04"))
		if c.Usage != nil {
			fmt.Fprintf(&b, "Списано сегодня по лимиту: %d из %d (осталось %d)\n", c.Usage.Used, c.Usage.Limit, c.Usage.Left)
		}
		fmt.Fprintf(&b, "Платежи: %d на %d⭐", a.PaymentsReal, a.StarsPaid)
		if a.PaymentsBypass > 0 {
			fmt.Fprintf(&b, " (+%d тестовых покупок админа)", a.PaymentsBypass)
		}
		b.WriteString("\n")
	}
	if c.Usage != nil {
		fmt.Fprintf(&b, "\n⭐ Подписка: %s", c.Usage.Plan.Title)
		if !c.Usage.Plan.IsFree() {
			fmt.Fprintf(&b, " до %s", c.Usage.ExpiresAt.In(loc).Format("02.01.2006 15:04"))
			left := time.Until(c.Usage.ExpiresAt)
			if left > 0 {
				fmt.Fprintf(&b, " (ещё %d дн.)", int(left.Hours()/24))
			}
		}
		if c.Usage.Stored != nil && !c.Usage.Plan.IsFree() {
			src := "оплата Stars"
			if c.Usage.Stored.Source == billing.SourceAdmin {
				src = "выдана админом"
			}
			fmt.Fprintf(&b, " · %s", src)
		}
		fmt.Fprintf(&b, "\nДневной лимит: %d\n", c.Usage.Limit)
	} else {
		b.WriteString("\n⭐ Подписки выключены\n")
	}
	if len(c.WeakTopics) > 0 {
		b.WriteString("\n🎯 Слабые темы:\n")
		for _, s := range c.WeakTopics {
			var ts []string
			for _, t := range s.Topics {
				ts = append(ts, t.Topic)
			}
			fmt.Fprintf(&b, "• %s: %s\n", s.Subject, strings.Join(ts, ", "))
		}
	} else {
		b.WriteString("\n🎯 Слабых тем нет\n")
	}
	if len(c.Actions) > 0 {
		b.WriteString("\n🛠 Последние действия админов:\n")
		for _, a := range c.Actions {
			fmt.Fprintf(&b, "• %s %s (админ %d) %s\n", a.CreatedAt.In(loc).Format("02.01 15:04"), a.Action, a.AdminTgID, a.Details)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// --- Plan change -----------------------------------------------------------------------

func (h *Handler) showPlanPicker(ctx context.Context, cb *bot.CallbackQuery, userID int64) {
	if h.billing == nil {
		h.sendText(ctx, cb.Message.Chat.ID, "Подписки выключены (SUBSCRIPTIONS_ENABLED=0)")
		return
	}
	c, err := h.admin.Card(ctx, userID)
	if err != nil {
		logf("admin plan picker %d: %v", userID, err)
		h.sendText(ctx, cb.Message.Chat.ID, "Ошибка загрузки 😔")
		return
	}
	idS := strconv.FormatInt(userID, 10)
	cur := c.Usage.Plan
	text := fmt.Sprintf("⭐ Подписка пользователя %s\n\nСейчас: %s", c.User.DisplayName(), cur.Title)
	if !cur.IsFree() {
		text += " до " + c.Usage.ExpiresAt.In(h.admin.Location()).Format("02.01.2006 15:04")
	}
	text += "\n\nВыбери новый план (✅ — текущий). Изменение применяется сразу после подтверждения."
	rows := [][]bot.InlineKeyboardButton{}
	for _, p := range h.billing.Catalog().Plans() {
		label := p.Title
		if p.IsFree() {
			label = "Бесплатный (Free)"
		}
		if p.Code == cur.Code {
			label = "✅ " + label + " — текущий"
		}
		if p.IsFree() {
			rows = append(rows, bot.Row(bot.Btn(label, cbAdmPlanAsk+idS+":"+p.Code+":0")))
		} else {
			rows = append(rows, bot.Row(bot.Btn(fmt.Sprintf("%s · %d/день", label, p.DailyLimit), cbAdmPlanDays+idS+":"+p.Code)))
		}
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ К карточке", cbAdmUser+idS)))
	h.editMessage(ctx, cb, text, &bot.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// parsePlanArgs parses "userID:plan[:days]".
func parsePlanArgs(s string, withDays bool) (userID int64, plan string, days int, ok bool) {
	f := strings.Split(s, ":")
	if (withDays && len(f) != 3) || (!withDays && len(f) != 2) {
		return 0, "", 0, false
	}
	id, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || id <= 0 || f[1] == "" {
		return 0, "", 0, false
	}
	if withDays {
		d, err := strconv.Atoi(f[2])
		if err != nil || d < 0 || d > 3660 {
			return 0, "", 0, false
		}
		days = d
	}
	return id, f[1], days, true
}

func (h *Handler) showPlanTerms(ctx context.Context, cb *bot.CallbackQuery, args string) {
	userID, plan, _, ok := parsePlanArgs(args, false)
	if !ok || h.billing == nil {
		return
	}
	p, found := h.billing.Catalog().Get(plan)
	if !found {
		h.sendText(ctx, cb.Message.Chat.ID, "Неизвестный план")
		return
	}
	idS := strconv.FormatInt(userID, 10)
	var row []bot.InlineKeyboardButton
	for _, d := range admPlanTerms {
		row = append(row, bot.Btn(fmt.Sprintf("%d дн.", d), fmt.Sprintf("%s%s:%s:%d", cbAdmPlanAsk, idS, p.Code, d)))
	}
	h.editMessage(ctx, cb, fmt.Sprintf("⭐ %s (%d прохождений в день)\n\nНа какой срок выдать? Срок считается от текущего момента.", p.Title, p.DailyLimit),
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{row, bot.Row(bot.Btn("⬅️ Назад", cbAdmPlan+idS))}})
}

func (h *Handler) askPlanChange(ctx context.Context, cb *bot.CallbackQuery, args string) {
	userID, plan, days, ok := parsePlanArgs(args, true)
	if !ok || h.billing == nil {
		return
	}
	p, found := h.billing.Catalog().Get(plan)
	if !found {
		h.sendText(ctx, cb.Message.Chat.ID, "Неизвестный план")
		return
	}
	u, err := h.admin.Repo().UserBriefByID(ctx, userID)
	if err != nil {
		h.sendText(ctx, cb.Message.Chat.ID, "Пользователь не найден")
		return
	}
	idS := strconv.FormatInt(userID, 10)
	text := fmt.Sprintf("Подтверди: снять платную подписку у %s (%d) и перевести на Free?", u.DisplayName(), u.TelegramID)
	if !p.IsFree() {
		until := time.Now().Add(time.Duration(days) * 24 * time.Hour).In(h.admin.Location())
		text = fmt.Sprintf("Подтверди: выдать %s на %d дн. (до %s) пользователю %s (%d)?\n\nТекущий план будет заменён сразу.",
			p.Title, days, until.Format("02.01.2006 15:04"), u.DisplayName(), u.TelegramID)
	}
	h.editMessage(ctx, cb, text, &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("✅ Подтвердить", fmt.Sprintf("%s%s:%s:%d", cbAdmPlanApply, idS, p.Code, days)),
			bot.Btn("❌ Отмена", cbAdmPlan+idS)),
	}})
}

func (h *Handler) applyPlanChange(ctx context.Context, cb *bot.CallbackQuery, args string) {
	userID, plan, days, ok := parsePlanArgs(args, true)
	if !ok {
		h.answerCallback(ctx, cb, "")
		return
	}
	g, err := h.admin.SetPlanDetailed(ctx, cb.From.ID, userID, plan, days)
	switch {
	case errors.Is(err, services.ErrNotAdmin):
		h.answerCallback(ctx, cb, "")
		return
	case errors.Is(err, services.ErrUnknownPlan):
		h.answerCallback(ctx, cb, "Неизвестный план")
		return
	case err != nil:
		logf("admin set plan %d %s %d: %v", userID, plan, days, err)
		h.answerCallback(ctx, cb, "Ошибка: план не изменён")
		return
	}
	note := "✅ Пользователь переведён на Free"
	if p, ok := h.billing.Catalog().Get(plan); ok && !p.IsFree() {
		note = fmt.Sprintf("✅ Выдан %s до %s", p.Title, g.Until.In(h.admin.Location()).Format("02.01.2006"))
	}
	h.answerCallback(ctx, cb, note)
	h.showUserCard(ctx, cb, cb.Message.Chat.ID, userID)
	if extra := strings.TrimSpace(grantStarsNote(g)); extra != "" {
		h.sendText(ctx, cb.Message.Chat.ID, "🛠 "+extra)
	}
}

// --- Broadcast wizard -------------------------------------------------------------------

// bcDraft is the broadcast being composed (admin_sessions.data).
type bcDraft struct {
	Text        string
	Entities    string // raw JSON
	MediaType   string
	MediaFileID string
}

func draftFrom(sess *repositories.AdminSession) bcDraft {
	str := func(k string) string {
		if v, ok := sess.Data[k].(string); ok {
			return v
		}
		return ""
	}
	return bcDraft{Text: str("text"), Entities: str("entities"), MediaType: str("media_type"), MediaFileID: str("media_file_id")}
}

func (d bcDraft) data() map[string]any {
	return map[string]any{"text": d.Text, "entities": d.Entities, "media_type": d.MediaType, "media_file_id": d.MediaFileID}
}

// mediaOf extracts a photo / video of a message ("" when none).
func mediaOf(m *bot.Message) (kind, fileID string) {
	switch {
	case len(m.Photo) > 0:
		return "photo", m.Photo[len(m.Photo)-1].FileID
	case m.Video != nil && m.Video.FileID != "":
		return "video", m.Video.FileID
	}
	return "", ""
}

func (h *Handler) bcTakeText(ctx context.Context, m *bot.Message, sess *repositories.AdminSession) {
	d := bcDraft{}
	if kind, file := mediaOf(m); kind != "" {
		d.MediaType, d.MediaFileID, d.Text = kind, file, m.Caption
		if len(m.CaptionEntities) > 0 {
			d.Entities = string(m.CaptionEntities)
		}
	} else {
		d.Text = m.Text
		if len(m.Entities) > 0 {
			d.Entities = string(m.Entities)
		}
	}
	if strings.TrimSpace(d.Text) == "" && d.MediaType == "" {
		h.sendText(ctx, m.Chat.ID, "Нужен текст (или фото/видео с подписью). Пришли ещё раз.")
		return
	}
	if err := services.ValidateBroadcast(&repositories.Broadcast{Text: d.Text, MediaType: d.MediaType, MediaFileID: d.MediaFileID}); err != nil {
		h.sendText(ctx, m.Chat.ID, "⚠️ "+err.Error()+"\n\nПришли текст ещё раз.")
		return
	}
	if d.MediaType != "" {
		if !h.setAdminSession(ctx, m.From.ID, admStateBcButtons, d.data()) {
			return
		}
		h.bcAskButtons(ctx, m.Chat.ID)
		return
	}
	if !h.setAdminSession(ctx, m.From.ID, admStateBcMedia, d.data()) {
		return
	}
	h.sendWithKeyboard(ctx, m.Chat.ID, "🖼 Шаг 2/3. Пришли фото или видео для рассылки — текст станет подписью (до 1024 символов). Или нажми «Без медиа».",
		bot.Row(bot.Btn("➡️ Без медиа", cbAdmBcNoMedia)), bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
}

func (h *Handler) bcTakeMedia(ctx context.Context, m *bot.Message, sess *repositories.AdminSession) {
	d := draftFrom(sess)
	kind, file := mediaOf(m)
	if kind == "" {
		h.sendWithKeyboard(ctx, m.Chat.ID, "Это не фото и не видео. Пришли фото/видео или нажми «Без медиа».",
			bot.Row(bot.Btn("➡️ Без медиа", cbAdmBcNoMedia)), bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
		return
	}
	d.MediaType, d.MediaFileID = kind, file
	if strings.TrimSpace(m.Caption) != "" {
		// A caption sent with the media replaces the text of step 1.
		d.Text, d.Entities = m.Caption, string(m.CaptionEntities)
	}
	if err := services.ValidateBroadcast(&repositories.Broadcast{Text: d.Text, MediaType: d.MediaType, MediaFileID: d.MediaFileID}); err != nil {
		h.sendWithKeyboard(ctx, m.Chat.ID, "⚠️ "+err.Error(),
			bot.Row(bot.Btn("➡️ Без медиа", cbAdmBcNoMedia)), bot.Row(bot.Btn("✏️ Начать заново", cbAdmBcNew)))
		return
	}
	if !h.setAdminSession(ctx, m.From.ID, admStateBcButtons, d.data()) {
		return
	}
	h.bcAskButtons(ctx, m.Chat.ID)
}

func (h *Handler) bcSkipMedia(ctx context.Context, tgID, chatID int64) {
	sess, err := h.admin.Repo().GetSession(ctx, tgID)
	if err != nil || sess.State != admStateBcMedia {
		h.sendText(ctx, chatID, "Черновик рассылки не найден — начни заново.")
		return
	}
	if !h.setAdminSession(ctx, tgID, admStateBcButtons, draftFrom(sess).data()) {
		return
	}
	h.bcAskButtons(ctx, chatID)
}

func (h *Handler) bcAskButtons(ctx context.Context, chatID int64) {
	h.sendWithKeyboard(ctx, chatID, "🔘 Шаг 3/3. Кнопки-ссылки под сообщением (необязательно).\n\nПришли по одной кнопке в строке в формате:\nТекст кнопки | https://ссылка\n\nНесколько кнопок в одном ряду — через ;;\nНапример:\nНаш канал | https://t.me/juz40\nСайт | https://example.com ;; Чат | https://t.me/juz40chat",
		bot.Row(bot.Btn("➡️ Без кнопок", cbAdmBcNoBtn)), bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
}

func (h *Handler) bcTakeButtons(ctx context.Context, m *bot.Message, sess *repositories.AdminSession) {
	rows, err := services.ParseBroadcastButtons(m.Text)
	if err != nil {
		h.sendWithKeyboard(ctx, m.Chat.ID, "⚠️ "+err.Error()+"\n\nПришли кнопки ещё раз или нажми «Без кнопок».",
			bot.Row(bot.Btn("➡️ Без кнопок", cbAdmBcNoBtn)), bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
		return
	}
	h.bcPreview(ctx, m.From.ID, m.Chat.ID, rows)
}

// bcPreview stores the draft, sends the real message to the admin (exactly
// what users will get) and asks for the confirmation.
func (h *Handler) bcPreview(ctx context.Context, tgID, chatID int64, buttons [][]repositories.BroadcastButton) {
	sess, err := h.admin.Repo().GetSession(ctx, tgID)
	if err != nil || (sess.State != admStateBcButtons) {
		h.sendText(ctx, chatID, "Черновик рассылки не найден — начни заново.")
		return
	}
	d := draftFrom(sess)
	b := &repositories.Broadcast{Text: d.Text, MediaType: d.MediaType, MediaFileID: d.MediaFileID, Buttons: buttons}
	if d.Entities != "" && d.Entities != "null" {
		b.Entities = []byte(d.Entities)
	}
	draft, err := h.admin.CreateBroadcastDraft(ctx, tgID, b)
	if err != nil {
		logf("broadcast draft: %v", err)
		h.sendText(ctx, chatID, "⚠️ Не удалось сохранить черновик: "+err.Error())
		return
	}
	data := d.data()
	data["draft_id"] = draft.ID
	if !h.setAdminSession(ctx, tgID, admStateBcPreview, data) {
		return
	}
	h.sendText(ctx, chatID, "👀 Предпросмотр — так сообщение увидят пользователи:")
	if _, err := h.tg.SendBroadcast(ctx, chatID, services.BroadcastMessage(draft)); err != nil {
		logf("broadcast preview: %v", err)
		h.sendWithKeyboard(ctx, chatID, "⚠️ Telegram не принял сообщение: "+err.Error()+"\n\nИсправь текст/кнопки и начни заново.",
			bot.Row(bot.Btn("✏️ Начать заново", cbAdmBcNew)), bot.Row(bot.Btn("❌ Отмена", cbAdmAbort)))
		return
	}
	n, err := h.admin.Repo().CountBroadcastAudience(ctx)
	if err != nil {
		logf("broadcast audience: %v", err)
	}
	idS := strconv.FormatInt(draft.ID, 10)
	h.sendWithKeyboard(ctx, chatID, fmt.Sprintf("Отправить эту рассылку? Получателей: %d (все, кроме заблокировавших бота).", n),
		bot.Row(bot.Btn(fmt.Sprintf("✅ Отправить (%d)", n), cbAdmBcSend+idS)),
		bot.Row(bot.Btn("✏️ Начать заново", cbAdmBcNew), bot.Btn("❌ Отмена", cbAdmAbort)))
}

func (h *Handler) bcConfirm(ctx context.Context, cb *bot.CallbackQuery, id int64) {
	changed, total, err := h.admin.ConfirmBroadcast(ctx, cb.From.ID, id)
	if err != nil {
		logf("broadcast confirm %d: %v", id, err)
		h.answerCallback(ctx, cb, "Ошибка, рассылка не запущена")
		return
	}
	if !changed {
		h.answerCallback(ctx, cb, "Эта рассылка уже запущена или отменена")
		h.bcCard(ctx, cb, cb.Message.Chat.ID, id)
		return
	}
	h.answerCallback(ctx, cb, "🚀 Рассылка запущена")
	h.clearAdminSession(ctx, cb.From.ID)
	if h.bcWaker != nil {
		h.bcWaker.Wake()
	}
	idS := strconv.FormatInt(id, 10)
	h.editMessage(ctx, cb, fmt.Sprintf("🚀 Рассылка #%d запущена: %d получателей. Отправка идёт в фоне — по окончании пришлю итог.", id, total),
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(bot.Btn("📈 Прогресс", cbAdmBcCard+idS), bot.Btn("⛔ Остановить", cbAdmBcCancel+idS)),
			bot.Row(bot.Btn("⬅️ Админка", cbAdmMenu)),
		}})
}

func (h *Handler) bcCancel(ctx context.Context, cb *bot.CallbackQuery, id int64) {
	changed, err := h.admin.CancelBroadcast(ctx, cb.From.ID, id)
	if err != nil {
		logf("broadcast cancel %d: %v", id, err)
		h.answerCallback(ctx, cb, "Ошибка")
		return
	}
	if changed {
		h.answerCallback(ctx, cb, "⛔ Рассылка остановлена")
	} else {
		h.answerCallback(ctx, cb, "Рассылка уже завершена")
	}
	h.bcCard(ctx, cb, cb.Message.Chat.ID, id)
}

var bcStatusTitle = map[string]string{
	repositories.BroadcastDraft:    "черновик",
	repositories.BroadcastQueued:   "в очереди",
	repositories.BroadcastSending:  "отправляется",
	repositories.BroadcastDone:     "завершена",
	repositories.BroadcastCanceled: "остановлена",
}

func (h *Handler) bcList(ctx context.Context, cb *bot.CallbackQuery) {
	list, err := h.admin.Repo().ListBroadcasts(ctx, 10)
	if err != nil {
		logf("broadcast list: %v", err)
		h.sendText(ctx, cb.Message.Chat.ID, "Ошибка загрузки 😔")
		return
	}
	loc := h.admin.Location()
	text := "📜 Прошлые рассылки (последние 10)"
	if len(list) == 0 {
		text += "\n\nРассылок ещё не было."
	}
	rows := [][]bot.InlineKeyboardButton{}
	for _, b := range list {
		rows = append(rows, bot.Row(bot.Btn(fmt.Sprintf("#%d · %s · %s · %d/%d", b.ID, b.CreatedAt.In(loc).Format("02.01 15:04"),
			bcStatusTitle[b.Status], b.Sent, b.Total), cbAdmBcCard+strconv.FormatInt(b.ID, 10))))
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ Рассылка", cbAdmBc)))
	h.editMessage(ctx, cb, text, &bot.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func snippet(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func (h *Handler) bcCard(ctx context.Context, cb *bot.CallbackQuery, chatID, id int64) {
	b, err := h.admin.Repo().BroadcastByID(ctx, id)
	if err != nil {
		h.sendText(ctx, chatID, "Рассылка не найдена")
		return
	}
	c, err := h.admin.Repo().CountRecipients(ctx, id)
	if err != nil {
		logf("broadcast counts %d: %v", id, err)
	}
	h.editMessage(ctx, cb, renderBroadcastCard(b, c, h.admin.Location(), time.Now()), bcCardKeyboard(b))
}

func bcCardKeyboard(b *repositories.Broadcast) *bot.InlineKeyboardMarkup {
	idS := strconv.FormatInt(b.ID, 10)
	rows := [][]bot.InlineKeyboardButton{}
	if b.Status == repositories.BroadcastQueued || b.Status == repositories.BroadcastSending {
		rows = append(rows, bot.Row(bot.Btn("🔄 Обновить", cbAdmBcCard+idS), bot.Btn("⛔ Остановить", cbAdmBcCancel+idS)))
	}
	rows = append(rows, bot.Row(bot.Btn("📜 Все рассылки", cbAdmBcList), bot.Btn("⬅️ Админка", cbAdmMenu)))
	return &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// renderBroadcastCard renders the progress / result of a broadcast.
func renderBroadcastCard(b *repositories.Broadcast, c repositories.BroadcastCounts, loc *time.Location, now time.Time) string {
	var s strings.Builder
	fmt.Fprintf(&s, "📣 Рассылка #%d — %s\n", b.ID, bcStatusTitle[b.Status])
	fmt.Fprintf(&s, "Создана: %s (админ %d)\n", b.CreatedAt.In(loc).Format("02.01.2006 15:04"), b.AdminTgID)
	media := "только текст"
	switch b.MediaType {
	case "photo":
		media = "фото + подпись"
	case "video":
		media = "видео + подпись"
	}
	btns := 0
	for _, r := range b.Buttons {
		btns += len(r)
	}
	fmt.Fprintf(&s, "Содержимое: %s, кнопок: %d\n«%s»\n\n", media, btns, snippet(b.Text, 120))
	total := c.Total
	if total == 0 {
		total = b.Total
	}
	fmt.Fprintf(&s, "Получателей: %d\n✅ Отправлено: %d\n⛔ Заблокировали бота: %d\n⚠️ Ошибки: %d\n", total, c.Sent, c.Blocked, c.Failed)
	if c.Pending+c.Sending > 0 {
		fmt.Fprintf(&s, "⏳ Осталось: %d\n", c.Pending+c.Sending)
	}
	if c.Canceled > 0 {
		fmt.Fprintf(&s, "⛔ Не отправлено (остановлена): %d\n", c.Canceled)
	}
	if b.StartedAt != nil {
		end := now
		if b.FinishedAt != nil {
			end = *b.FinishedAt
		}
		fmt.Fprintf(&s, "Время: %s", billing.HumanDuration(end.Sub(*b.StartedAt)))
		if b.FinishedAt == nil && c.Done() > 0 && c.Pending > 0 {
			rate := float64(c.Done()) / end.Sub(*b.StartedAt).Seconds()
			if rate > 0 {
				fmt.Fprintf(&s, " · осталось ≈ %s", billing.HumanDuration(time.Duration(float64(c.Pending)/rate)*time.Second))
			}
		}
	}
	return strings.TrimRight(s.String(), "\n")
}

// BroadcastFinished implements services.BroadcastNotifier: the final
// report goes to the admin who created the broadcast.
func (h *Handler) BroadcastFinished(ctx context.Context, b *repositories.Broadcast) {
	c := repositories.BroadcastCounts{Total: b.Total, Sent: b.Sent, Failed: b.Failed, Blocked: b.Blocked}
	loc := time.UTC
	if h.admin != nil {
		loc = h.admin.Location()
	}
	text := "🏁 Рассылка завершена\n\n" + renderBroadcastCard(b, c, loc, time.Now())
	if _, err := h.tg.SendMessage(ctx, b.AdminTgID, text, bcCardKeyboard(b)); err != nil {
		logf("broadcast report to %d: %v", b.AdminTgID, err)
	}
}

// --- Admin-bypass purchases ---------------------------------------------------------------

// canBypassPay: the purchase of this Telegram user is applied without an
// invoice (administrator, billing on). The service re-checks ADMIN_IDS.
func (h *Handler) canBypassPay(tgUserID int64) bool {
	return h.billing != nil && h.isAdminTG(tgUserID) && h.billing.IsAdmin(tgUserID)
}

// bypassPurchase applies an administrator's purchase at once and shows
// the same result a real payment shows.
func (h *Handler) bypassPurchase(ctx context.Context, chatID int64, user *models.User, tgUserID int64, payload string) {
	out, err := h.billing.AdminBypassPurchase(ctx, user.ID, tgUserID, payload)
	var refused *services.BypassRefusedError
	switch {
	case errors.As(err, &refused):
		h.sendText(ctx, chatID, "🛠 Тестовая покупка отклонена: "+refused.Reason)
		return
	case err != nil:
		logf("admin bypass purchase %s tg %d: %v", payload, tgUserID, err)
		h.sendText(ctx, chatID, "🛠 Тестовая покупка не удалась — подробности в логах сервера.")
		return
	}
	if h.admin != nil {
		h.admin.LogAction(ctx, tgUserID, services.ActionBypassPurchase, user.ID, "payload="+payload+" outcome="+out.Kind)
	}
	h.sendText(ctx, chatID, "🛠 Режим администратора: покупка проведена без оплаты (тестовая, звёзды не списаны).")
	h.sendPaymentOutcome(ctx, chatID, user, out)
}

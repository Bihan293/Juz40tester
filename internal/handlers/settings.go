package handlers

import (
	"context"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
)

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

const settingsText = "⚙️ Настройки\n\n🌐 Язык тестов\n\nВыберите язык, на котором показываются вопросы и варианты ответов. Интерфейс бота остаётся на русском.\n\nЕсли казахской версии теста ещё нет, я переведу его один раз и сохраню — дальше она откроется мгновенно.\n\nℹ️ Языковые предметы (русский, английский, казахский язык, литература) не переводятся — их тесты всегда на языке самого предмета."

// showSettings is used from the Reply Keyboard (new message).
func (h *Handler) showSettings(ctx context.Context, chatID int64, user *models.User) {
	if _, err := h.tg.SendMessage(ctx, chatID, settingsText, settingsKeyboard(user.TestLang)); err != nil {
		logf("send settings: %v", err)
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
		logf("set test lang %d -> %q: %v", user.ID, lang, err)
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

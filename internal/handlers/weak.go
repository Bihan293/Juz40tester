package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// --- Weak-topics (personal tests) ------------------------------------------------

// renderWeakMenu renders the weak-topics subject picker (main menu ->
// "🎯 Слабые темы"). Only subjects in which the user ACTUALLY has weak
// (🔴/🟡) topics are listed: a subject the user never practised cannot have
// a weak topic, and offering it led to an endless «generating…» state. No
// generation is triggered here — a test is created only on an explicit tap.
func (h *Handler) renderWeakMenu(ctx context.Context, user *models.User) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, weak, err := h.quiz.WeakMenu(ctx, user.ID, weakMenuTopics)
	if err != nil {
		return "", nil, err
	}
	if len(subjects) == 0 {
		return "🎯 Слабые темы\n\nПока у тебя нет слабых тем 🙌\n\nТема становится слабой, когда ты несколько раз ошибаешься в ней (по всем пройденным тестам), и уходит отсюда, когда начинаешь отвечать уверенно. Загляни в «📚 Предметы» — а потом возвращайся сюда за персональным тестом.",
			&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
				bot.Row(bot.Btn("📚 К предметам", cbSubjects)),
				bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
			}}, nil
	}
	var b strings.Builder
	b.WriteString("🎯 Слабые темы\n\nТемы, в которых ты систематически ошибаешься (по всем твоим ответам, последние 10 по каждой теме):\n🔴 — верно меньше половины, 🟡 — верно меньше 80%.\n")
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbWeakSubject+strconv.FormatInt(s.ID, 10)))
		fmt.Fprintf(&b, "\n%s %s\n", subjectEmoji(s.Name), s.Name)
		for _, st := range weak[s.ID] {
			n, c := st.WindowCounts()
			fmt.Fprintf(&b, "%s %s — верно %d из %d\n", models.TopicLevelEmoji(st.Level()), st.Topic, c, n)
		}
	}
	b.WriteString("\nВыбери предмет — я соберу персональный тест из 20 НОВЫХ вопросов по самым слабым темам:")
	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	text := b.String()
	text = truncateUTF16(text, weakMenuMaxUTF16)
	return text, &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// weakMenuTopics is how many weak topics per subject the picker lists (the
// personal test itself trains the 5 worst ones).
const weakMenuTopics = 5

// showWeakMenu is used from the Reply Keyboard (new message).
func (h *Handler) showWeakMenu(ctx context.Context, chatID int64, user *models.User) {
	text, kb, err := h.renderWeakMenu(ctx, user)
	if err != nil {
		logf("weak menu: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки 😔", nil); err != nil {
			logf("send weak menu error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send weak menu: %v", err)
	}
}

// editWeakMenu is used from an inline button (edits the current message).
func (h *Handler) editWeakMenu(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	text, kb, err := h.renderWeakMenu(ctx, user)
	if err != nil {
		logf("weak menu: %v", err)
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
	if errors.Is(err, services.ErrPersonalGenLimit) {
		h.answerAlert(ctx, cb, "На сегодня лимит новых персональных тестов исчерпан 🙏 Пройди уже готовые тесты в «📚 Предметы» — завтра соберу новый тест по слабым темам.")
		return
	}
	if errors.Is(err, services.ErrGenQueueBusy) {
		h.answerAlert(ctx, cb, "Сейчас очень много желающих — очередь генерации заполнена 🙏 Попробуй через пару минут, а пока пройди тесты в «📚 Предметы».")
		return
	}
	if err != nil {
		logf("personal test %d/%d: %v", user.ID, subjectID, err)
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
		// R-7: personal watchers are keyed by the job (job:<id>). Without a
		// personal job the user waits for topic_batch jobs (B4b): the
		// watcher re-runs the bank assembly (kicked when a batch of the
		// subject finishes), which also covers the tiny window where a
		// personal job has already finished.
		key := bankWatchKey(subjectID, user.ID)
		check := func(ctx context.Context) (*models.Test, bool, error) {
			test, pending, _, err := h.quiz.EnsurePersonalTest(ctx, user.ID, subjectID)
			if errors.Is(err, services.ErrPersonalGenLimit) || errors.Is(err, services.ErrGenQueueBusy) {
				return nil, false, nil
			}
			return test, pending, err
		}
		if jobID, jerr := h.quiz.ActivePersonalJobID(ctx, user.ID, subjectID); jerr != nil {
			logf("personal job of %d/%d: %v", user.ID, subjectID, jerr)
		} else if jobID > 0 {
			key = jobWatchKey(jobID)
			check = func(ctx context.Context) (*models.Test, bool, error) {
				testID, pending, err := h.quiz.JobState(ctx, jobID)
				if err != nil || testID == 0 {
					return nil, pending, err
				}
				test, err := h.quiz.GetTest(ctx, testID)
				return test, false, err
			}
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

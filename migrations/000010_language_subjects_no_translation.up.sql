-- Language subjects (Русский язык, Английский язык, Казахский язык,
-- литература, иностранные языки) must NEVER be machine-translated: their
-- questions and answer options are words/forms of the studied language, so
-- a Kazakh translation of e.g. a Russian spelling task has no valid answer.
-- The application now skips translation for such subjects; this migration
-- removes the broken Kazakh translations that were cached earlier.
-- (Pure cache cleanup — the original master question rows are untouched.)
--
-- Patterns are listed in both lower- and capitalised form: lower()/ILIKE on
-- Cyrillic depends on the database locale (C locale does not fold it).
DELETE FROM question_translations tr
USING questions q, subjects s
WHERE tr.question_id = q.id
  AND q.subject_id = s.id
  AND EXISTS (
      SELECT 1 FROM (VALUES
          ('%русск%'), ('%Русск%'), ('%РУССК%'),
          ('%орыс%'), ('%Орыс%'),
          ('%казахский%'), ('%Казахский%'), ('%казахская%'), ('%Казахская%'),
          ('%қазақ тілі%'), ('%Қазақ тілі%'), ('%қазақ әдебиеті%'), ('%Қазақ әдебиеті%'),
          ('%английск%'), ('%Английск%'), ('%ағылшын%'), ('%Ағылшын%'),
          ('%english%'), ('%English%'),
          ('%немецк%'), ('%Немецк%'), ('%француз%'), ('%Француз%'),
          ('%иностранн%'), ('%Иностранн%'), ('%шет тілі%'), ('%Шет тілі%'),
          ('%литератур%'), ('%Литератур%'), ('%әдебиет%'), ('%Әдебиет%')
      ) AS p(pattern)
      WHERE s.name LIKE p.pattern
  );

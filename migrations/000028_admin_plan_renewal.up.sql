-- Admin plan changes vs. Telegram Stars subscriptions (docs/ADMIN.md,
-- «Изменить подписку»). Only ADDITIVE columns with defaults — safe on a
-- database with 000001…000027 applied.
--
--   sub_canceled_at — the bot cancelled the auto-renewal of the Telegram
--     subscription sub_charge_id (editUserStarSubscription is_canceled)
--     because an administrator changed the plan. Cleared when a new Stars
--     subscription is stored.
--   offer_plan — an administrator granted this plan and no Telegram
--     subscription renews it: when the granted period ends the user gets
--     a subscription link of this plan (once — offer_sent_at).
ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS sub_canceled_at TIMESTAMPTZ;
ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS offer_plan TEXT NOT NULL DEFAULT '';
ALTER TABLE user_subscriptions ADD COLUMN IF NOT EXISTS offer_sent_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_user_subs_offer_due ON user_subscriptions (expires_at)
    WHERE offer_plan <> '' AND offer_sent_at IS NULL AND status = 'active';

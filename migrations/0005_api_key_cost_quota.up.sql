ALTER TABLE "console_api_keys" ADD COLUMN "cost_quota_microusd" bigint;
ALTER TABLE "console_api_keys" ADD COLUMN "cost_used_microusd" bigint DEFAULT 0 NOT NULL;
ALTER TABLE "console_requests" ADD COLUMN "quota_charged_microusd" bigint DEFAULT 0 NOT NULL;
ALTER TABLE "console_api_keys" DROP COLUMN "token_quota";
ALTER TABLE "console_api_keys" DROP COLUMN "token_used";
ALTER TABLE "console_requests" DROP COLUMN "quota_charged_tokens";
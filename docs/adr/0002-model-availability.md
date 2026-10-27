# Model availability after provider failures

A model-specific `503` after retries takes that Model offline for all Customers. The offline state persists in SQLite across restarts, offline Models disappear from `/v1/models`, and direct calls receive `503`. The gateway checks an offline Model with a one-token request immediately, then after 1, 5, 15, and every 30 minutes until one succeeds; probe costs belong to the operator. Provider-caused failures and undelivered responses are not charged or recorded as Served requests.

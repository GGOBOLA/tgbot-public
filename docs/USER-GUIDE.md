# How to use the AI API dashboard 🛰

Open the bot your operator gave you in Telegram and send `/start`. Your account
is created automatically on the first start.

Everything works with buttons on one dashboard message. Press **⬅️ Menu** to
come back at any time.

## The menu

| Button | What it does |
|---|---|
| 💰 Balance | Your current credit in RM |
| 📊 Usage | Requests, tokens, spend, active keys + 🔍 per-key breakdown |
| 🔑 Generate API Key | Creates a new API key |
| 🗝 My Keys | Your active keys, with a revoke button on each |
| 🏷 Price list | Every model with the prices per 1M tokens |
| 🔄 Reload Balance | Pay by DuitNow QR and claim your payment |

## 1. Get your API key

1. Tap **🔑 Generate API Key**.
2. The key looks like `sk-` + 64 letters/numbers.
3. **Copy it immediately. It is shown only once.** Nobody (not even the admin)
   can see it again. Lose it? Tap revoke on the old key and generate a new one.
4. You can hold up to **5 active keys** at a time. Give each one a short
   name (like "Claude Code") so My Keys is easy to read.

## 2. Call the API

The API is OpenAI-compatible. The admin gives you the base URL — it is whatever
the gateway is published on, ending in `/v1`:

```
<gateway base URL>/v1/chat/completions
```

Authorization header: `Authorization: Bearer <your API key>`.

- Available models: same base URL, `/v1/models`. Models temporarily offline are
  omitted; direct calls to them receive `503` until the upstream recovers.
- Your balance is charged per token at the prices on the 🏷 Price list button.
- One request can use several small attempts internally; you are only charged
  for tokens actually served.

## 3. Check your usage

Tap **📊 Usage** for lifetime totals and the last 7 days (requests, tokens,
spend, active keys) and
**🔍 Per-key breakdown** to see which of your keys spent what. Usage appears
once your key starts calling the gateway.

## 4. Top up your balance

1. Tap **🔄 Reload Balance**. The DuitNow QR appears as a photo.
2. Scan and pay with your banking app.
3. **Tap the button for the exact amount you paid** (RM 10 / RM 20 / RM 50).
   That opens a payment claim and tells the admin.
4. Wait for the admin to check the statement (usually minutes). You get a
   receipt message and the credit lands on your balance right away.

- Paid a different amount, or lost the QR? Message the admin.
- Do not tap a claim button twice for one payment. One tap per payment.

## 5. Balance warnings

- At **RM 2.00 remaining** you get one warning message.
- At **RM 0.00** your keys stop working (HTTP 402). Top up to resume;
  no need to make a new key.
- Spending is deducted per request as it happens, so numbers are live.

## 6. Manage keys

**🗝 My Keys** lists your active keys under the names you gave them. Tap
**❌ Revoke** to kill one instantly if
it leaked or you retired it. Revoked keys stop working immediately and cannot
be restored. Revoking frees a slot below the 5-key limit.

## Tips

- Your account is your Telegram ID, shown on the dashboard as
  `Customer ID`. Mention it when contacting the admin.
- The dashboard edits one message, so your chat stays tidy.
- Questions or problems? Message the admin directly.

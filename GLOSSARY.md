# tgbot

A Telegram dashboard and API gateway where customers buy model usage with account credit. The operator manages access, prices, and top-up claims.

## People and access

**Customer**:
A person who uses a Telegram identity to hold credit and API access. A customer may also have the Admin role.
_Avoid_: User, account holder

**Admin**:
A Telegram identity authorized to operate the service, including reviewing top-up claims and setting customer prices. An Admin may also be a Customer.
_Avoid_: Operator account

**API key**:
A credential that lets a Customer call the gateway. A Customer’s keys use the same balance and request allowance.
_Avoid_: Account key

## Credit and usage

**Balance**:
A Customer’s current credit for model usage. Charges from requests already in flight may take it below zero; new requests stop when it is zero or lower.
_Avoid_: Wallet

**Top-up claim**:
A Customer’s request for an Admin to verify an external payment and add the corresponding credit to the Customer’s Balance.
_Avoid_: Payment, deposit

**Customer price**:
The rate the service charges a Customer for model usage, separate from the amount the upstream charges the operator.
_Avoid_: Provider cost

**Served request**:
A successful model response delivered to the Customer. Its reported usage is charged at the Customer price; when usage is missing, the service uses an estimate.
_Avoid_: Provider attempt

## Model availability

**Model**:
A model made available through the gateway when it has a Customer price and is online at the upstream.
_Avoid_: Provider

**Upstream**:
The external service that receives model requests from the gateway and may charge the operator for them.
_Avoid_: Customer-facing provider

# In-flight request billing

The gateway checks credit before forwarding a request but does not reserve it. Calls already in flight may complete and take the Customer’s Balance below zero; new calls are refused at zero or below. This accepts a temporary amount owed to avoid dropping work already sent upstream.

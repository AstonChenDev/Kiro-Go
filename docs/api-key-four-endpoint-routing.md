# API Key four-endpoint routing

API Key accounts can opt into a four-endpoint upstream fallback chain without
changing OAuth accounts or the legacy Kiro CLI runtime path.

## Enablement

The default is `legacy`, which preserves the existing single-runtime behavior.

```sh
KIRO_APIKEY_ROUTE_MODE=four
```

For a one-account canary, provide an exact comma-separated account-ID list:

```sh
KIRO_APIKEY_ROUTE_MODE=four
KIRO_APIKEY_ROUTE_ACCOUNT_IDS=account-id-1,account-id-2
```

Unset `KIRO_APIKEY_ROUTE_ACCOUNT_IDS` to enable the route for every API Key
account. Set `KIRO_APIKEY_ROUTE_MODE=legacy` or unset it to roll back without a
code or data change.

## Attempt order

One account and one immutable serialized request body are used for the entire
chain:

1. `q.{region}.amazonaws.com/generateAssistantResponse`, no `x-amz-target`
2. `runtime.{region}.kiro.dev/generateAssistantResponse`, no `x-amz-target`
3. CodeWhisperer host and target; outside `us-east-1`, use the regional Q host
4. Regional Q host with the Amazon Q `SendMessage` target

The router advances only after a transport failure, HTTP 408, HTTP 429, or HTTP
5xx. Request/authentication/quota errors such as 400, 401, 402, and 403 stop the
chain. Any HTTP 2xx response is consumed immediately and later endpoints are
not contacted.

## Compatibility adapter

The isolated route normalizes the IDE envelope once before serialization. It
fills missing agent task, continuation, trigger, and conversation fields while
preserving values already supplied by the caller. This is required because the
Claude translator already emits the IDE agent fields, while the legacy OpenAI
translator does not.

Some API Key responses from the Q/IDE endpoint contain valid content frames
and end cleanly but omit the optional metadata `stopReason`. The route keeps
streaming those content frames immediately and, only after the frame decoder
reaches a clean end, supplies `end_turn` (or `tool_use`) to the downstream
adapter. A real upstream stop reason is always preserved. Empty, malformed, or
errored 2xx streams are not treated as successful output and are never replayed
on a later endpoint.

## Verification

Normal tests never access the network or credentials:

```sh
go test -count=1 ./...
go test -race -count=1 ./proxy ./pool
go vet ./...
```

An opt-in release smoke test is available under the `live` build tag. Inject
the credential into the test process; do not store it in the repository:

```sh
KIRO_LIVE_API_KEY=... \
KIRO_LIVE_ACCOUNT_ID=... \
KIRO_LIVE_REGION=us-east-1 \
go test -tags=live -run TestLiveAPIKeyFourEndpointRoute -v ./proxy
```

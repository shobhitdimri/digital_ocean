# Feature Flag Service: API Test Guide

Live deployment: `https://ffs-go-feature-flags-9rnac.ondigitalocean.app`

## Contents

1. [Setup](#1-setup)
2. [Automated test script](#2-automated-test-script)
3. [Health checks](#3-health-checks)
4. [Flags: create, read, update, delete](#4-flags-create-read-update-delete)
5. [Evaluation and rule order](#5-evaluation-and-rule-order)
6. [Per-user overrides](#6-per-user-overrides)
7. [Percentage rollout](#7-percentage-rollout)
8. [Flag dependencies](#8-flag-dependencies)
9. [Audit trail](#9-audit-trail)
10. [Error responses](#10-error-responses)
11. [Cleanup](#11-cleanup)
12. [Notes about this deployment](#12-notes-about-this-deployment)

## Endpoint summary

| Method and path | Purpose | Success |
| --- | --- | --- |
| `GET /healthz` | liveness | 200 |
| `GET /readyz` | readiness | 200 |
| `POST /v1/flags` | create a flag | 201 |
| `GET /v1/flags` | list flags | 200 |
| `GET /v1/flags/{name}` | read one flag | 200 |
| `PUT /v1/flags/{name}` | partial update | 200 |
| `DELETE /v1/flags/{name}` | delete a flag | 204 |
| `PUT /v1/flags/{name}/overrides/{userId}` | set a user override | 200 |
| `DELETE /v1/flags/{name}/overrides/{userId}` | remove a user override | 204 |
| `GET /v1/evaluate?flag=X&user_id=U` | evaluate a flag for a user | 200 |
| `GET /v1/flags/{name}/audit?limit=50` | change history, newest first | 200 |

Headers: every write (POST, PUT, DELETE) needs `X-Actor`, otherwise the response is `400 missing_actor`. `X-Request-ID` is optional and is echoed back.

## 1. Setup

Run this once in your shell. The rest of the guide uses `$U` and `${W[@]}`.

```bash
U=https://ffs-go-feature-flags-9rnac.ondigitalocean.app
W=(-H 'X-Actor: me' -H 'Content-Type: application/json')
```

## 2. Automated test script

Needs `bash`, `curl` and `jq`. It cleans up its own test flags.

```bash
git clone https://github.com/shobhitdimri/digital_ocean.git && cd digital_ocean
BASE_URL=$U ./sample_test/integration_test.sh
```

## 3. Health checks

### 3.1 Liveness

```bash
curl -i $U/healthz        # 200 {"status":"ok"}
```

### 3.2 Readiness

```bash
curl -i $U/readyz         # 200 {"status":"ready"}
```

## 4. Flags: create, read, update, delete

### 4.1 Create a flag

A new flag starts disabled unless you send `enabled:true`.

```bash
curl -i -X POST $U/v1/flags "${W[@]}" -d '{"name":"new-ui","description":"Redesign","enabled":true}'   # 201 + Location header
```

### 4.2 Create a duplicate

```bash
curl -i -X POST $U/v1/flags "${W[@]}" -d '{"name":"new-ui"}'      # 409 already_exists
```

### 4.3 Create with an invalid name

```bash
curl -i -X POST $U/v1/flags "${W[@]}" -d '{"name":"bad name"}'    # 400 invalid_request (no spaces)
```

### 4.4 Write without X-Actor

```bash
curl -i -X POST $U/v1/flags -H 'Content-Type: application/json' -d '{"name":"x"}'   # 400 missing_actor
```

### 4.5 List flags

```bash
curl $U/v1/flags          # 200 {"flags":[...]} sorted by name
```

### 4.6 Get one flag

```bash
curl $U/v1/flags/new-ui   # 200, shows version, rollout_percent, requires
```

### 4.7 Get an unknown flag

```bash
curl -i $U/v1/flags/nope  # 404 (never 500)
```

### 4.8 Update a flag (partial)

Only the fields you send change.

```bash
curl -X PUT $U/v1/flags/new-ui "${W[@]}" -d '{"enabled":false}'   # version 1 -> 2
```

### 4.9 Repeat an update (no-op)

```bash
curl -X PUT $U/v1/flags/new-ui "${W[@]}" -d '{"enabled":false}'   # version stays 2, no audit entry
```

### 4.10 Update with an empty body

```bash
curl -i -X PUT $U/v1/flags/new-ui "${W[@]}" -d '{}'               # 400 nothing to update
```

## 5. Evaluation and rule order

Rules run in this order:

1. A flag's prerequisites must be on for that user, otherwise it is off.
2. A per-user override wins.
3. A globally disabled flag is off.
4. A rollout below 100% includes only some users.
5. Otherwise the flag is on.

### 5.1 Evaluate a flag

```bash
curl "$U/v1/evaluate?flag=new-ui&user_id=alice"
# {"flag":"new-ui","user_id":"alice","enabled":false,"reason":"global_off","source":"store"}
```

### 5.2 Evaluate again (served from cache)

```bash
curl "$U/v1/evaluate?flag=new-ui&user_id=alice"    # same answer, "source":"cache"
```

### 5.3 Evaluate an unknown flag

```bash
curl -i "$U/v1/evaluate?flag=nope&user_id=alice"   # 404
```

### 5.4 Evaluate without user_id

```bash
curl -i "$U/v1/evaluate?flag=new-ui"               # 400
```

## 6. Per-user overrides

### 6.1 Set an override

```bash
curl -X PUT $U/v1/flags/new-ui/overrides/alice "${W[@]}" -d '{"enabled":true}'
```

### 6.2 Check the override takes effect

```bash
curl "$U/v1/evaluate?flag=new-ui&user_id=alice"    # enabled:true, reason user_override, source store (cache invalidated)
curl "$U/v1/evaluate?flag=new-ui&user_id=bob"      # still global_off
```

### 6.3 Remove an override

```bash
curl -i -X DELETE $U/v1/flags/new-ui/overrides/alice "${W[@]}"    # 204
```

### 6.4 Remove it again

```bash
curl -i -X DELETE $U/v1/flags/new-ui/overrides/alice "${W[@]}"    # 404 (already gone)
```

## 7. Percentage rollout

The hash is fixed, so for a flag named exactly `checkout` the results are predictable. alice is at position 410 out of 10,000, and bob is at 5030.

### 7.1 Create a 50% rollout

```bash
curl -X POST $U/v1/flags "${W[@]}" -d '{"name":"checkout","enabled":true,"rollout_percent":50}'
```

### 7.2 Evaluate two known users

```bash
curl "$U/v1/evaluate?flag=checkout&user_id=alice"   # enabled:true  reason rollout_included
curl "$U/v1/evaluate?flag=checkout&user_id=bob"     # enabled:false reason rollout_excluded
```

### 7.3 Raise the rollout to 51%

```bash
curl -X PUT $U/v1/flags/checkout "${W[@]}" -d '{"rollout_percent":51}'
curl "$U/v1/evaluate?flag=checkout&user_id=bob"     # now included (5030 < 5100)
```

### 7.4 Out-of-range percentage

```bash
curl -i -X PUT $U/v1/flags/checkout "${W[@]}" -d '{"rollout_percent":101}'    # 400
```

### 7.5 Full rollout

```bash
curl -X PUT $U/v1/flags/checkout "${W[@]}" -d '{"rollout_percent":100}'       # reason becomes global_on
```

### 7.6 Distribution check

Roughly half of 100 users should be on at 50%.

```bash
curl -X PUT $U/v1/flags/checkout "${W[@]}" -d '{"rollout_percent":50}' >/dev/null
for i in $(seq 1 100); do curl -s "$U/v1/evaluate?flag=checkout&user_id=u$i"; done | grep -c '"enabled":true'
```

## 8. Flag dependencies

### 8.1 Create a flag with a prerequisite

```bash
curl -X POST $U/v1/flags "${W[@]}" -d '{"name":"payments","enabled":true}'
curl -X POST $U/v1/flags "${W[@]}" -d '{"name":"checkout-v2","enabled":true,"requires":["payments"]}'
curl "$U/v1/evaluate?flag=checkout-v2&user_id=alice"           # enabled:true
```

### 8.2 Turn the prerequisite off

```bash
curl -X PUT $U/v1/flags/payments "${W[@]}" -d '{"enabled":false}'
curl "$U/v1/evaluate?flag=checkout-v2&user_id=alice"           # false, reason prerequisite_unmet
```

### 8.3 An override cannot bypass a prerequisite

```bash
curl -X PUT $U/v1/flags/checkout-v2/overrides/alice "${W[@]}" -d '{"enabled":true}'
curl "$U/v1/evaluate?flag=checkout-v2&user_id=alice"           # still false
```

### 8.4 Cycle is rejected

```bash
curl -i -X PUT $U/v1/flags/payments "${W[@]}" -d '{"requires":["checkout-v2"]}'   # 409, message shows the loop
```

### 8.5 Unknown prerequisite is rejected

```bash
curl -i -X POST $U/v1/flags "${W[@]}" -d '{"name":"z","requires":["ghost"]}'      # 400
```

### 8.6 Cannot delete a required flag

```bash
curl -i -X DELETE $U/v1/flags/payments "${W[@]}"               # 409 (checkout-v2 requires it)
```

### 8.7 Clear prerequisites, then delete

```bash
curl -X PUT $U/v1/flags/checkout-v2 "${W[@]}" -d '{"requires":[]}'
curl -i -X DELETE $U/v1/flags/payments "${W[@]}"               # 204
```

## 9. Audit trail

### 9.1 Read the history

```bash
curl "$U/v1/flags/new-ui/audit?limit=10"
# newest first: who (X-Actor), when, action, and full before/after snapshots
```

### 9.2 Request id is echoed

```bash
curl -H 'X-Request-ID: my-trace-1' -i $U/v1/flags/new-ui | grep -i x-request-id
```

## 10. Error responses

Every error has the same shape, including unknown routes and wrong methods.

### 10.1 Unknown route

```bash
curl -i $U/v1/nothing            # 404 {"error":{"code":"not_found","message":...,"request_id":...}}
```

### 10.2 Wrong method

```bash
curl -i -X PATCH $U/v1/flags     # 405 method_not_allowed, with an Allow header
```

### 10.3 Wrong content type

```bash
curl -i -X POST $U/v1/flags "${W[@]}" -H 'Content-Type: text/plain' -d 'x'   # 415
```

## 11. Cleanup

Overrides go with their flag. Delete the dependent flag before the one it requires.

```bash
for f in checkout-v2 payments checkout new-ui; do curl -s -o /dev/null -w "$f %{http_code}\n" -X DELETE $U/v1/flags/$f "${W[@]}"; done
```

## 12. Notes about this deployment

- No login. `X-Actor` is only a label recorded in the audit trail, so anyone with the URL can write.
- Data resets when the app restarts or redeploys.
- The first request after idle can be slow by a few seconds on this small instance.

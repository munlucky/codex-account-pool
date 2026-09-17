# Google Antigravity 실제 로그인 및 Live 테스트 가이드

대상: Windows 10/11 + Docker Desktop + PowerShell

이 문서는 `google-antigravity` OAuth profile을 실제 Google 계정으로 등록한 뒤, GPT Codex Router의 `/v1` 경로와 Qwen Code까지 확인하는 수동 Live 검증 절차다.

> 실제 계정과 upstream quota를 사용하는 테스트다. 본인이 사용할 권한이 있는 계정만 사용하고, 토큰·redirect URL·`auth.json` 내용을 로그/이슈/채팅에 붙여넣지 않는다.

## 0. 검증 범위

이 절차가 성공하면 다음을 확인한 것이다.

1. Google OAuth authorization code + PKCE 로그인이 성공한다.
2. access/refresh token과 Cloud Code Assist project binding이 동일 profile에 저장된다.
3. `/v1/models`에서 `google-antigravity/*` 모델을 조회할 수 있다.
4. `/v1/responses` non-stream 생성이 실제 Antigravity upstream까지 왕복한다.
5. SSE streaming이 `response.completed`까지 끝난다.
6. function call 2턴이 성공해 `thoughtSignature` replay 경로가 실제 upstream에서 수용된다.
7. Qwen Code `openai-responses` provider가 같은 `/v1` endpoint로 Antigravity 모델을 사용할 수 있다.

`/v1/models`는 live discovery 실패 시 bounded static fallback을 사용하므로 **모델 목록 조회만 성공했다고 Live upstream 검증 성공으로 판정하지 않는다.** 최소한 4번 실제 생성 테스트까지 성공해야 한다.

---

## 1. 최신 코드로 컨테이너 재빌드

PowerShell:

```powershell
cd C:\dev\ai-auth-proxy

git status --short

docker compose down
docker compose up -d --build

docker compose ps
Invoke-WebRequest http://127.0.0.1:8317/healthz
```

정상 기준:

```text
StatusCode : 200
...
ok
```

기존 설치에서 `.env`가 없다면 Compose가 `GPT_CODEX_ROUTER_STATE_ROOT`를 요구한다. 기존 Codex profile이 있는 환경에서는 **기존 state root를 그대로 사용한다.** 빈 새 state root를 임의로 만들지 않는다.

현재 state root가 Compose에 연결됐는지만 확인하려면:

```powershell
docker compose config
```

출력에 토큰은 없어야 한다. 실제 credential 파일 내용은 열어보지 않는다.

---

## 2. 첫 Antigravity profile 로그인

첫 profile 이름을 `google-1`로 예시한다.

```powershell
docker compose exec -it gpt-codex-router `
  gpt-codex-router auth add google-antigravity google-1
```

CLI에 다음과 비슷한 안내가 나온다.

```text
Open this URL in your browser to sign in with Google Antigravity:
https://accounts.google.com/...
```

출력된 Google 로그인 URL을 브라우저에서 연다. 계정을 선택하고 권한 승인까지 진행한다.

### Docker에서 callback 페이지가 연결 실패하는 경우

현재 기본 redirect URI는 다음이다.

```text
http://127.0.0.1:51121/callback
```

Docker 모드에서는 컨테이너 내부 loopback callback을 의도적으로 사용하지 않는다. 따라서 브라우저가 마지막 redirect에서 `127.0.0.1:51121` 연결 실패 화면을 보이는 것이 정상이다. 이 경우 **실패 화면의 주소창에 남아 있는 전체 redirect URL**을 복사한다.

예시 형태만 보면 다음과 같다.

```text
http://127.0.0.1:51121/callback?state=...&code=...
```

그 전체 URL을 처음 실행해 둔 `docker compose exec -it ... auth add ...` 터미널에 붙여넣고 Enter를 누른다.

`state`와 `code` 값 자체를 따로 기록하거나 공유하지 않는다. CLI는 붙여넣은 URL의 `state`가 현재 로그인 transaction과 일치하는지 검증한다.

정상 완료 기준:

```text
Google Antigravity login completed for profile "google-1".
Registered google-antigravity/google-1. ...
```

로그인 실패 시 profile registry 등록은 완료되지 않아야 한다. 같은 이름으로 `auth add`를 다시 시도할 수 있다.

---

## 3. 인증 상태 검증

```powershell
docker compose exec -T gpt-codex-router `
  gpt-codex-router auth status google-antigravity google-1

docker compose exec -T gpt-codex-router `
  gpt-codex-router auth list
```

정상 예시:

```text
Google Antigravity profile google-1: authenticated
```

첫 번째 `google-antigravity` profile이면 provider의 ACTIVE 값도 `yes`여야 한다.

기존 profile을 다시 로그인해야 할 때는 `add`가 아니라 다음 명령을 사용한다.

```powershell
docker compose exec -it gpt-codex-router `
  gpt-codex-router auth login google-antigravity google-1
```

`auth list`와 `auth status`에는 access token, refresh token, Google email, account ID, Cloud Code Assist project ID가 출력되면 안 된다.

---

## 4. `/v1/models` 확인

router local API key를 가져온다.

```powershell
$key = (docker compose exec -T gpt-codex-router `
  gpt-codex-router api-key).Trim()

$headers = @{
  Authorization = "Bearer $key"
}
```

모델 목록 조회:

```powershell
$catalog = Invoke-RestMethod `
  -Method Get `
  -Uri "http://127.0.0.1:8317/v1/models" `
  -Headers $headers

$agModels = @(
  $catalog.data |
    Where-Object { $_.id -like "google-antigravity/*" }
)

$agModels | Format-Table id, owned_by
```

테스트 모델은 `gemini-3.8-flash`가 보이면 우선 사용하고, 없으면 목록의 첫 Antigravity 모델을 사용한다.

```powershell
$model = ($agModels |
  Where-Object { $_.id -eq "google-antigravity/gemini-3.8-flash" } |
  Select-Object -First 1).id

if (-not $model) {
  $model = ($agModels | Select-Object -First 1).id
}

if (-not $model) {
  throw "No google-antigravity model is visible from /v1/models"
}

"TEST MODEL: $model"
```

주의: 여기까지는 static catalog fallback일 수 있으므로 Live 성공 판정이 아니다.

---

## 5. 실제 non-stream Responses 생성

```powershell
$body = @{
  model = $model
  input = "Reply with exactly: pong"
  stream = $false
  reasoning = @{
    effort = "low"
  }
} | ConvertTo-Json -Depth 10 -Compress

$response = Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:8317/v1/responses" `
  -Headers $headers `
  -ContentType "application/json" `
  -Body $body

$response | ConvertTo-Json -Depth 20
```

assistant text만 보기:

```powershell
$response.output |
  Where-Object { $_.type -eq "message" } |
  ForEach-Object { $_.content } |
  ForEach-Object { $_ } |
  Where-Object { $_.type -eq "output_text" } |
  ForEach-Object { $_.text }
```

정상 기준:

```text
pong
```

이 단계가 성공하면 local `/v1` 인증, provider prefix routing, Google credential loading, Cloud Code Assist project binding, CCA upstream 호출, CCA→Responses 변환까지 실제 왕복한 것이다.

---

## 6. SSE streaming 테스트

```powershell
$streamBody = @{
  model = $model
  input = "Count from 1 to 3, one number per line."
  stream = $true
} | ConvertTo-Json -Depth 10 -Compress

curl.exe -N "http://127.0.0.1:8317/v1/responses" `
  -H "Authorization: Bearer $key" `
  -H "Content-Type: application/json" `
  --data-binary $streamBody
```

정상 스트림에는 최소한 다음 이벤트 계열이 보여야 한다.

```text
response.created
response.output_item.added
response.output_text.delta
...
response.completed
```

HTTP 200만 받고 `response.completed`가 없으면 성공으로 판정하지 않는다.

---

## 7. function call + thoughtSignature replay Live 테스트

이 테스트가 Antigravity agent 호환성에서 중요하다. 첫 턴에서 upstream function call을 받고, 두 번째 턴에서 동일 call history + tool result를 다시 보내 실제 `thoughtSignature` replay가 수용되는지 확인한다.

### 7-1. 첫 턴: function call 생성

```powershell
$tool = @{
  type = "function"
  name = "get_test_value"
  description = "Return the fixed test value supplied by the caller."
  parameters = @{
    type = "object"
    properties = @{}
    additionalProperties = $false
  }
}

$firstInput = @(
  @{
    type = "message"
    role = "user"
    content = @(
      @{
        type = "input_text"
        text = "Call get_test_value exactly once, then use its result as the final answer."
      }
    )
  }
)

$firstBody = @{
  model = $model
  input = $firstInput
  tools = @($tool)
  tool_choice = @{
    type = "function"
    name = "get_test_value"
  }
  stream = $false
} | ConvertTo-Json -Depth 30 -Compress

$first = Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:8317/v1/responses" `
  -Headers $headers `
  -ContentType "application/json" `
  -Body $firstBody

$call = $first.output |
  Where-Object { $_.type -eq "function_call" } |
  Select-Object -First 1

if (-not $call) {
  $first | ConvertTo-Json -Depth 30
  throw "Expected function_call was not returned"
}

$call | Format-List
```

정상 기준: `call_id`, `name`, `arguments`를 가진 `function_call`이 1개 이상 나온다.

### 7-2. 두 번째 턴: function result replay

첫 사용자 메시지를 동일하게 포함해야 fallback session anchor가 유지된다.

```powershell
$secondInput = @(
  $firstInput[0],
  @{
    type = "function_call"
    call_id = $call.call_id
    name = $call.name
    arguments = $call.arguments
  },
  @{
    type = "function_call_output"
    call_id = $call.call_id
    output = "TEST_OK"
  }
)

$secondBody = @{
  model = $model
  input = $secondInput
  tools = @($tool)
  stream = $false
} | ConvertTo-Json -Depth 30 -Compress

$second = Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:8317/v1/responses" `
  -Headers $headers `
  -ContentType "application/json" `
  -Body $secondBody

$second | ConvertTo-Json -Depth 30
```

assistant text 확인:

```powershell
$second.output |
  Where-Object { $_.type -eq "message" } |
  ForEach-Object { $_.content } |
  ForEach-Object { $_ } |
  Where-Object { $_.type -eq "output_text" } |
  ForEach-Object { $_.text }
```

정상 기준:

- HTTP 400 `missing thought_signature`가 발생하지 않는다.
- 요청이 `response.completed` 의미로 끝난다.
- 최종 응답이 tool result `TEST_OK`를 사용한다.

이 테스트가 실패하면서 upstream 오류가 `thought_signature`를 언급하면 signature replay 경로를 우선 점검한다.

---

## 8. 구조화 로그 확인

최근 10분 provider 로그만 확인한다.

```powershell
docker compose logs --since 10m gpt-codex-router |
  Select-String '"provider":"google-antigravity"'
```

정상적으로 다음 lifecycle이 관측되어야 한다.

```text
request_start
upstream_attempt
request_end
```

그리고 `request_end`가 정상 생성에서 다음 의미를 가져야 한다.

```text
"semantic_outcome":"completed"
```

로그에 prompt 본문, response 본문, access token, refresh token, authorization code, project ID가 노출되면 안 된다.

---

## 9. Qwen Code 연결

먼저 현재 router key 값을 확인한다.

```powershell
$key = (docker compose exec -T gpt-codex-router `
  gpt-codex-router api-key).Trim()
$key
```

`C:\Users\<사용자>\.qwen\settings.json`의 기존 내용을 보존하면서 Antigravity 모델 entry를 추가한다.

예시:

```json
{
  "$version": 4,
  "env": {
    "GPT_CODEX_ROUTER_API_KEY": "gcr_REPLACE_WITH_CURRENT_ROUTER_KEY"
  },
  "modelProviders": {
    "openai-responses": [
      {
        "id": "google-antigravity/gemini-3.8-flash",
        "name": "Gemini 3.8 Flash (Antigravity via GPT Codex Router)",
        "envKey": "GPT_CODEX_ROUTER_API_KEY",
        "baseUrl": "http://127.0.0.1:8317/v1",
        "generationConfig": {
          "timeout": 120000
        }
      }
    ]
  },
  "security": {
    "auth": {
      "selectedType": "openai-responses"
    }
  },
  "model": {
    "name": "google-antigravity/gemini-3.8-flash"
  }
}
```

실제 `$model` 값이 다른 경우 JSON의 `id`와 `model.name`을 그 값으로 맞춘다.

Qwen 프로세스를 완전히 종료한 뒤 새 PowerShell에서 실행한다.

```powershell
qwen -p "Reply with exactly: pong"
```

정상 기준:

```text
pong
```

### Qwen tool 호출까지 확인

프로젝트 루트에서 다음처럼 실제 파일 읽기를 요구한다.

```powershell
cd C:\dev\ai-auth-proxy
qwen -p "Use your file-reading tool to read go.mod and reply with only the module path. Do not guess."
```

정상 기준:

```text
github.com/munlucky/codex-account-pool
```

이 테스트는 단순 text generation보다 실제 agent/tool path에 가깝다. Qwen의 approval 설정에 따라 tool 사용 승인이 필요할 수 있다.

---

## 10. 두 번째 Google 계정 테스트 - 선택 사항

두 번째 계정을 등록한다.

```powershell
docker compose exec -it gpt-codex-router `
  gpt-codex-router auth add google-antigravity google-2
```

수동 선택:

```powershell
docker compose exec -T gpt-codex-router `
  gpt-codex-router auth use google-antigravity google-2

docker compose exec -T gpt-codex-router `
  gpt-codex-router auth status google-antigravity google-2
```

그 뒤 5번의 non-stream 테스트를 다시 실행해 `google-2` credential로 실제 생성이 되는지 확인한다.

429 failover을 검증하려고 의도적으로 quota를 소진하지 않는다. 구현상 generic 429는 계정을 바꾸지 않고, 현재 profile의 **정확한 wire model quota가 0으로 확인되고** 다른 profile의 동일 model quota가 양수로 확인되는 경우에만 다른 credential snapshot으로 한 번 replay한다.

---

## 11. 실패 시 진단 순서

### 로그인 URL 승인 후 token exchange 실패

증상 예:

```text
Antigravity token request failed: HTTP 4xx
```

확인:

1. redirect URL 전체를 잘라먹지 않고 붙여넣었는지 확인한다.
2. 이전 로그인에서 받은 URL/code를 재사용하지 않는다.
3. 새 `auth add` 또는 기존 profile이면 `auth login`으로 OAuth transaction을 처음부터 다시 시작한다.
4. 로컬 `.env`의 `GOOGLE_ANTIGRAVITY_CLIENT_ID` / `GOOGLE_ANTIGRAVITY_CLIENT_SECRET` 값이 현재 사용할 OAuth client와 일치하는지 확인한다. 저장소에는 실제 OAuth client credential을 커밋하지 않는다.

### Cloud Code Assist project discovery 실패

증상 예:

```text
Antigravity login could not discover a Cloud Code Assist project for this account
```

이 경우 credential을 성공 profile로 등록하지 않는다. 해당 Google 계정이 Antigravity/Cloud Code Assist를 실제로 사용할 수 있는 계정인지 먼저 확인하고 새 로그인으로 재시도한다.

### `/v1/models`에는 나오지만 생성이 실패

모델 목록은 fallback catalog일 수 있다. 실제 오류 판단은 `/v1/responses`와 container lifecycle log를 기준으로 한다.

```powershell
docker compose logs --since 10m gpt-codex-router
```

### 401

동일 Google profile의 refresh token으로 한 번 refresh한 뒤 재시도한다. 그래도 401이면:

```powershell
docker compose exec -it gpt-codex-router `
  gpt-codex-router auth login google-antigravity google-1
```

### 429

일반적인 429에서 임의 계정 회전은 하지 않는다. 정확한 model quota 소진 증거가 없는 429는 그대로 반환하는 것이 정상 정책이다.

### function-call 2턴에서 400 thought_signature 오류

7번 테스트의 첫/두 번째 요청에서:

- 같은 model을 사용했는지,
- 첫 user message를 동일하게 유지했는지,
- 첫 response의 `call_id`, `name`, `arguments`를 바꾸지 않았는지 확인한다.

그 조건이 맞는데도 재현되면 Antigravity signature replay 결함으로 취급한다.

---

## 12. 최종 PASS 체크리스트

아래를 모두 만족하면 첫 Live 검증을 PASS로 볼 수 있다.

- [ ] `auth add google-antigravity google-1` 완료
- [ ] `auth status ...` = authenticated
- [ ] `auth list`에 credential 값 미노출
- [ ] `/v1/models`에 `google-antigravity/*` 노출
- [ ] non-stream Responses가 실제 text 생성 완료
- [ ] streaming Responses가 `response.completed`까지 완료
- [ ] function call 1턴 성공
- [ ] tool result를 포함한 2턴 성공, `missing thought_signature` 없음
- [ ] lifecycle log에 `provider=google-antigravity`, semantic completed 확인
- [ ] lifecycle log에 token/code/project/prompt/response 본문 미노출
- [ ] Qwen `qwen -p "Reply with exactly: pong"` 성공
- [ ] Qwen file-reading tool smoke test 성공
- [ ] 기존 Codex bare model `/v1` 요청도 회귀 없이 성공

마지막 항목은 provider router가 기존 Codex 동작을 침범하지 않았는지 확인하기 위한 필수 회귀 테스트다. 기존 Codex 모델 하나로 5번의 non-stream 테스트를 다시 실행하면 된다.

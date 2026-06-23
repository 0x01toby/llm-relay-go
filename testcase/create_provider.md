```shell
curl 'http://localhost:3300/__console/api/providers' \
  -H 'Accept: application/json' \
  -H 'Accept-Language: zh-CN,zh;q=0.9' \
  -H 'Connection: keep-alive' \
  -H 'Content-Type: application/json' \
  -b 'CONSOLE_COOKIE_NAME=v1:1cf70383' \
  -H 'Origin: http://localhost:3300' \
  -H 'Referer: http://localhost:3300/' \
  -H 'Sec-Fetch-Dest: empty' \
  -H 'Sec-Fetch-Mode: cors' \
  -H 'Sec-Fetch-Site: same-origin' \
  -H 'User-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36' \
  -H 'sec-ch-ua: "Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"' \
  -H 'sec-ch-ua-mobile: ?0' \
  -H 'sec-ch-ua-platform: "macOS"' \
  --data-raw '{"channelName":"sssapi","type":"anthropic","targetBaseUrl":"https://node-hk.sssaiapi.com","systemPrompt":null,"models":[{"model":"claude-opus-4-8"},{"model":"claude-opus-4-7"},{"model":"claude-sonnet-4-6"}],"priority":0,"routingVisibility":"direct","responsesMode":null,"extraFields":null,"auth":{"header":"authorization","value":"sk-sssaicode-a144e9af8e12dd5fe46839ae6a85b5c3f52c085822ef5a880170f34946874be3"}}'
```
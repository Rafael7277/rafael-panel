# iplimit.md — طراحی دروازه‌ی IP Limit با قوانین Blackhole پایدار

> **وضعیت Build 41:** پیاده‌سازی فعال در `forks/xray-core/app/iplimit` نشست‌های ورودیِ احرازشده را همان لحظه می‌شمارد. `limitIp` سقف IPهای متمایز با نشست زنده است؛ چند جریان از یک IP یک جایگاه می‌گیرند و IP تازه وقتی سقف پر باشد فوراً رد می‌شود. جایگاه با لغو context نشست آزاد می‌شود، نه با برگشت یک جریان مقصد؛ بنابراین UDP و multiplex هم طی عمر نشست حساب می‌شوند. کلاینت محدود با IP مبدا نامشخص هم رد می‌شود. فایل سیاست برای همه‌ی کلاینت‌ها، حتی نامحدودها، ورودی صریح و strict دارد؛ فایل گم‌شده، ناقص یا نامعتبر و کلاینت ناشناخته پذیرفته نمی‌شود. Build 41 grace قابل‌تنظیم ندارد. موتور لاگ فقط IPهای اخیر را برای نمایش نگه می‌دارد و در تصمیم اتصال یا kick نقشی ندارد. بخش‌های قدیمی پایین‌تر، طراحی و رفتار نسخه‌های قبلی‌اند.

> **وضعیت فاز ۱:** پیاده‌سازی و تست کامل شد (تگ گیت `v39-sticky-iplimit`، کامیت `ad65f12`) — چک‌لیست بخش ۱۲.
> **بخش‌های ۱۳ به بعد:** یادداشت‌های طراحی و تاریخچه‌ی پیاده‌سازی اولیه؛ وضعیت فعلی Build 41 در توضیح بالا آمده است.
> **هدف:** پس از شناسایی تخلف (تعداد IP فعال > `limitIp`)، IP خاطی برای **هر کانکشن جدید** در همان لحظه‌ی احراز هویت رد شود (blackhole)، بدون قطع شدن IPهای مجاز؛ و با رفع تخلف، رد شدن خودکار برداشته شود.
> **پایه:** موتور event-driven موجود در Build 39 (`web/service/client_limit_engine.go`) دست‌نخورده باقی می‌ماند؛ این طراحی یک **لایه‌ی اعمال** (enforcement) روی آن اضافه می‌کند.

---

## ۱. خلاصه‌ی طراحی در یک پاراگراف

موتور فعلی هر ۱ ثانیه access log را تیک می‌زند و state «هر ایمیل → IPهای فعال» را در حافظه نگه می‌دارد؛ هنگام تخلف کاربر را یک‌بار کیک می‌کند (RemoveUser + 100ms + AddUser) و سپس کاربر با همه‌ی IPها می‌تواند دوباره وصل شود. طراحی جدید همان تشخیص را نگه می‌دارد، ولی اعمال را از «کیکِ تکرارشونده» به «قانون routing پایدار» تغییر می‌دهد: برای هر (ایمیل، IPِ خاطی) یک قانون `user + source → blackhole` با gRPC در Xray نصب می‌شود، پس **هر اتصال جدیدِ آن IP هم در درِ احراز هویت blackhole می‌شود**. کانکشن established فعلیِ خاطی با همان کیکِ واحدِ فعلی قطع می‌شود. وقتی تعداد IPهای فعال زیر حد برگردد، قانون حذف می‌شود.

---

## ۲. آنچه در این تحقیق تأیید شد (زیربنای طراحی)

همه‌ی موارد زیر روی نسخه‌ی قفل‌شده‌ی Xray در پروژه (`github.com/xtls/xray-core v1.260327.0` — همان `go.mod`) بررسی و تأیید شده‌اند:

1. **`RoutingService` با `AddRule`/`RemoveRule` وجود دارد** — client آماده‌ی تولیدشده: `app/router/command.NewRoutingServiceClient(conn)` (`command_grpc.pb.go`، متدهای `AddRule`/`RemoveRule`).
2. **قانون routing با `user` + `source` به‌صورت AND مچ می‌شود** — `RoutingRule.UserEmail []string` (فیلد ۷) + `Source` (CIDR list) در `app/router/config.pb.go`؛ پارس JSON آن در `infra/conf/router.go:540-541` (`"user"`, `"source"`) و نگاشت `User → UserEmail` در خط ۶۳۷-۶۴۰.
3. **مرجع پیاده‌سازی رسمی خود Xray:** دستور `xray api sib` (Block connections by source IP) در `main/commands/all/api/source_ip_block.go` دقیقاً همین کار را می‌کند: JSON قاعده → `infra/conf` decode → `Build()` → `serial.ToTypedMessage` → `AddRule(ShouldAppend=true)`. ما همان الگو را تکرار می‌کنیم.
4. **outbound `blocked` (blackhole) از قبل در کانفیگ پیش‌فرض پنل وجود دارد** (`web/service/config.json` → outbounds) — نیازی به outbound جدید نیست.
5. **gRPC infrastructure پنل آماده است:** `xray/api.go` همین حالا `HandlerServiceClient` و `StatsServiceClient` را از یک `grpc.ClientConn` مشترک می‌سازد (`Init` خط ۷۰-۸۹) — اضافه کردن سومین client (`RoutingServiceClient`) چند خط است.

### ⚠️ کشف‌های بحرانی (محدودیت‌هایی که طراحی باید رعایت کند)

این‌ها را مستقیم از سورس Xray خواندم و رفتار طراحی را تعیین می‌کنند:

- **(الف) `AddRule` با `ShouldAppend=false` کل قوانین routing را پاک می‌کند** (`app/router/router.go:124-136`: `ReloadRules` بدون append، `r.rules` را از نو می‌سازد — یعنی قوانین استاتیک پنل مثل `inboundTag: api → api` هم پاک می‌شوند). **پس همیشه باید `ShouldAppend=true` بفرستیم.**
- **(ب) قوانین داینامیک به **انتهای** لیست اضافه می‌شوند (کم‌اولویت‌ترین).** قوانین پیش‌فرض پنل (api / geoip:private / bittorrent) ترافیک عادی کاربر را مچ نمی‌کنند، پس قانون ما عملاً همیشه اثر می‌کند. **محدودیت مستند:** اگر اپراتور قانون سفارشیِ جلوتر (مثلاً `domain: x → direct`) گذاشته باشد، برای مقصدهای آن قانون، قانونِ ما دیرتر ارزیابی می‌شود. قابل پذیرش است؛ در «محدودیت‌ها» ثبت می‌شود.
- **(ج) قوانین داینامیک فقط در حافظه‌ی Xray هستند — با restart ریست می‌شوند.** این مشکل نیست چون state موتور هم در حافظه است و با شروع لاگ‌ها دوباره ساخته می‌شود؛ به‌علاوه چرخه‌ی reconcile (بخش ۷) هر زمان قانونِ بدون تخلف ببیند حذفش می‌کند.
- **(د) `RoutingService` در کانفیگ API فعلی پنل فعال نیست:** آرایه‌ی `api.services` در `web/service/config.json` فقط `HandlerService, LoggerService, StatsService` دارد. سوئیچِ `infra/conf/api.go:37-41` (case `"routingservice"`) باید فعال شود — **این تغییر باید در کانفیگ در حال اجرا هم اعمال شود + Xray restart** (بخش ۸).
- **(هـ) `RemoveRule` با tag خالی خطا می‌دهد** (`router.go:213-229`) — همیشه tag یکتا می‌فرستیم.
- **(و) کشف‌شده حین پیاده‌سازی — ترتیب IPهای هم‌ثانیه باید deterministic باشد:** تایم‌استمپ‌های access.log دقت ثانیه‌ای دارند، پس دو IP یک کلاینت معمولاً `lastSeen` یکسان دارند. `sort.Slice` ناپایدار است و `excessIPs`، `violatingIPsLocked` و `applyPostKick` هرکدام روی همان map جداگانه iterate/sort می‌کنند — **روترِ چسبنده دُم لیست را blackhole می‌کند و `applyPostKick` سرِ لیست را نگه می‌دارد؛ اگر در یک pass اختلاف نظر داشته باشند، IPِ نگه‌داشته‌شده بلاک و IPِ اخراجی آزاد می‌شود.** راه‌حل: comparator مشترک `sightingNewer` (جدیدترین اول؛ tie-break روی رشته‌ی IP) در هر چهار محل sort.

---

## ۳. معماری

```
┌────────────────────────────────────────────────────────────────┐
│ web/service/client_limit_engine.go  (موجود — تغییرات حداقلی)  │
│                                                                │
│  tailOnce ──► observeLine ──► tracker (email → IPs + TTL)      │
│                                   │                            │
│  maintainOnce ──► تشخیص تخلف ────┤                            │
│                     (IPهای مجاز = جدیدترین limitIp تا)          │
│                                   ▼                            │
│              enforce / release  (نقاط اتصال جدید)               │
└───────────────┬────────────────────────────────┬───────────────┘
                │ Enforce(email, badIPs)         │ Release(email, ip)
                ▼                                ▼
┌────────────────────────────────────────────────────────────────┐
│ web/service/client_limit_router.go  (جدید)                     │
│                                                                │
│  rules: map[key]{email, ip, ruleTag, addedAt}                  │
│  Enforce  → AddRule(user=[email], source=[ip/32],              │
│             outboundTag=blocked, ruleTag=<key>, append=true)   │
│  Release  → RemoveRule(ruleTag=<key>)                          │
│  Reconcile/Restart → RemoveRule برای کلیدهای بی‌state           │
└───────────────┬───────────────────────────────────────────────┘
                │ (gRPC — همان ClientConn موجود)
                ▼
┌────────────────────────────────────────────────────────────────┐
│ xray/api.go  (توسعه)                                           │
│  + RoutingServiceClient                                        │
│  + AddRoutingRule(tag, email, ip) error                        │
│  + RemoveRoutingRule(tag) error                                │
└────────────────────────────────────────────────────────────────┘
```

### ۳.۱. چرا فایل جدید `client_limit_router.go`؟

جدا بودن از engine: تست‌پذیری مستقل (mock xrayAPI)، شفافیت لاگ/نظارت، و این که اگر سرویس router (مثلاً به‌دلیل خاموش بودن RoutingService) از کار بیفتد، engine بتواند به رفتار fallback (کیکِ فعلی) برگردد.

---

## ۴. جزئیات پیاده‌سازی

### ۴.۱. توسعه‌ی `xray/api.go`

```go
// در struct XrayAPI:
    RoutingServiceClient *routerCommand.RoutingServiceClient

// در Init() — بعد از ssClient:
    rsClient := routerCommand.NewRoutingServiceClient(conn)
    x.RoutingServiceClient = &rsClient

// در Close():
    x.RoutingServiceClient = nil

// متد جدید — ساخت قانون با الگوی رسمی xray api sib
// (JSON → infra/conf decode → Build → TypedMessage → AddRule):
func (x *XrayAPI) AddRoutingRule(ruleTag, email, ip string) error {
    // رشته‌ی JSON قاعده (بدون تزریق رشته — email/ip قبل از Marshal با
    // json.Marshal به JSON string امن تبدیل می‌شوند):
    //   {"routing":{"rules":[{"ruleTag":"<ruleTag>",
    //     "user":["<email>"],"source":["<ip>/32"],
    //     "outboundTag":"blocked"}]}}
    // سپس: serial.DecodeJSONConfig → (*conf.RouterConfig).Build()
    //       → cserial.ToTypedMessage → AddRuleRequest{ShouldAppend: true}
}

// متد جدید:
func (x *XrayAPI) RemoveRoutingRule(ruleTag string) error {
    // RemoveRuleRequest{RuleTag: ruleTag}
}
```

نکته‌های مهم:
- **`/32` دقیق** (نه CIDR عریض) تا همسایه‌های CGNAT مصون بمانند.
- email و ip با `json.Marshal` (نه `fmt.Sprintf` خام) داخل JSON قرار بگیرند تا escape امن باشد.
- timeout همان ۵ ثانیه‌ی pattern موجود `RemoveUser`.

### ۴.۲. فایل جدید `web/service/client_limit_router.go`

```go
type ClientLimitRouter struct {
    mu      sync.Mutex
    rules   map[string]*activeRule   // key: email + "|" + ip
    xrayAPI *xray.XrayAPI            // یا interface برای تست
}

type activeRule struct {
    Email   string
    IP      string
    RuleTag string    // "rafael-ipl-" + hex(sha256(email|ip))[:16] — یکتا و idempotent
    AddedAt time.Time
}
```

**RuleTag یکتا و deterministic:** به‌جای email خام داخل tag (که در لاگ systemd/ps قابل مشاهده و ممکن است فاصله/کاراکتر خاص داشته باشد)، هش کوتاهِ `(email|ip)` — idempotent است: فراخوانی دوباره‌ی Enforce برای همان جفت، قانون تکراری نمی‌سازد.

**متدها:**

```go
// Enforce — برای هر IP خاطی؛ خطای gRPC فقط لاگ می‌شود (fallback در engine)
func (r *ClientLimitRouter) Enforce(email string, badIPs []string)

// Release — حذف قانون یک (email, ip)
func (r *ClientLimitRouter) Release(email, ip string)

// ReleaseAll(email) — وقتی کلاینت حذف/غیرفعال شد یا limitIp برداشته شد
func (r *ClientLimitRouter) ReleaseAll(email string)

// Snapshot() []activeRule — برای endpoint نظارت (بخش ۱۰)
func (r *ClientLimitRouter) Snapshot() []activeRule
```

رفتار خطا: خطای `AddRule`/`RemoveRule` **fatal نیست** — لاگ می‌شود (`[LIMIT_RULE]` prefix، همان فایل لاگ موتور) و در تیک بعدی دوباره تلاش می‌شود (Enforce idempotent است چون `rules` map قبل از فراخوانی gRPC هم آپدیت می‌شود؛ اگر gRPC fail شد، رکورد در map علامت‌گذاری `pendingAdd=true` می‌شود و تیک بعدی retry می‌کند).

### ۴.۳. تغییرات در `client_limit_engine.go` (حداقلی)

فقط در `maintainOnce` (نقطه‌ی فعلی تشخیص تخلف و کیک):

1. محاسبه‌ی IPهای مجاز/خاطی — **همان سیاست فعلی** (`applyPostKick`): مجاز = جدیدترین `limitIp` تا IP فعال؛ خاطی = باقی.
2. برای هر IP خاطی: `limitRouter.Enforce(email, badIPs)`.
3. سپس همان کیکِ واحد فعلی اجرا می‌شود (برای قطعِ کانکشن established خاطی) — **رفتار پیش‌فرض حفظ می‌شود**.
4. **مسیر Release** (در همان maintainOnce): هر (email, ip) که در `rules` است ولی:
   - دیگر جزو IPهای فعال نیست (TTL تمام شده)، یا
   - activeCount ≤ limitIp شده، یا
   - آن IP جزو IPهای مجاز شده (مثلاً IP دیگری TTLش تمام شد و این یکی حالا مجاز است)
   
   → `limitRouter.Release(email, ip)`.
5. **قلاب‌های موجود save-path** (`clientLimitEngineNotifyClientUpdated` در `inbound.go:230,389,1865,2329,2360`): به‌علاوه‌ی notify فعلی، در صورت disable/حذف کلاینت یا صفر شدن `limitIp` → `ReleaseAll(email)`.

### ۴.۴. جریان کامل یک تخلف (walkthrough)

```
t=0    کاربر با IP-A وصل است (limitIp=1، فعال: {A})           → قانونی نیست
t=10   IP-B می‌آید؛ access log خط جدید                         → state: {A,B}
t=10   maintainOnce: activeCount=2 > 1 → تخلف
         مجاز = {B} (جدیدترین)؛ خاطی = {A}
         Enforce(email, [A]) → AddRule(user=[email], source=[A/32] → blocked)
         کیک واحد → کانکشن established A قطع، B خودکار reconnect
t=11+  هر اتصال جدید از A: در لحظه‌ی احراز هویت → blackhole     ✅ درِ بسته
       B بدون وقفه کار می‌کند                                   ✅
t=200  TTL A (پس از آخرین فعالیت) تمام → A از state خارج
       maintainOnce: A در rules هست ولی خاطی نیست → Release(A)
       (اگر A واقعاً رفته باشد، دیگر در access log نمی‌آید؛ اگر برگردد
        و باز هم تخلف باشد، چرخه از t=10 تکرار می‌شود)
```

---

## ۵. سازگاری با موتور فعلی (چه چیزی عوض نمی‌شود)

- `tailOnce` / `observeLine` / regexها / پارس policy (با فیکس unmarshal v39) — **دست‌نخورده**.
- TTL فعال بودن IP، policy cache، negative cache، grace (پیش‌فرض ۶۰s، حداقل مؤثر ۱۰s) — **دست‌نخورده**.
- `applyPostKick` (نگه‌داشتن جدیدترین IPها) — همان سیاست، حالا به‌عنوان مرزِ مجاز/خاطی.
- fail2ban وابستگی ندارد — همچنان.
- رفتار «کیک» در لحظه‌ی تشخیص — پیش‌فرض حفظ می‌شود (کانکشن established خاطی باید قطع شود).

---

## ۶. حالت fallback

اگر `RoutingService` روی Xray فعال نباشد (کانفیگ قدیمی) یا gRPC خطا بدهد:

- `AddRoutingRule` خطای «service not implemented / unavailable» برمی‌گرداند.
- router لاگ می‌کند و `pendingAdd` می‌ماند؛ **engine به رفتار فعلی (کیکِ تکرارشونده در هر تخلف) ادامه می‌دهد** — یعنی بدترین حالت، دقیقاً رفتار Build 39 فعلی است. هیچ regression ممکن نیست.
- تشخیص: اولین فراخوانی ناموفق یک warning یک‌باره لاگ می‌کند: «RoutingService not available — falling back to kick-only mode; add 'RoutingService' to api.services and restart Xray to enable sticky blocking».

---

## ۷. Reconcile و Restart

- **restart پنل:** `rules` map خالی می‌شود؛ Xray قوانین قبلی را هنوز دارد (اگر Xray restart نشده). راه‌حل: در `StartClientLimitEngine`، برای کلیدهایی که از state فعلی ساخته می‌شوند، Enforce idempotent اجرا می‌شود؛ قوانین یتیم (orphans) وقتی کاربرشان دیگر تخلفی ندارد، با اولین Release حذف می‌شوند. برای پاک‌سازی یتیم‌های فوری: چون RuleTag deterministic است، Enforce دوباره همان tag را می‌سازد (Xray قوانین با tag تکراری را append می‌کند — باید قبل از Add، `RemoveRule` همان tag فرستاده شود تا تکرار نسازد؛ هزینه‌اش یک RPC اضافی در هر Enforce است که قابل قبول است).
- **restart Xray:** قوانین پاک می‌شوند؛ state موتور باقی می‌ماند؛ تیک بعدی برای تخلف‌های همچنان فعال دوباره Enforce می‌کند. (فاصله‌ی یک-دو تیک که در آن IP خاطی می‌تواند وصل شود — همان رفتار today.)

---

## ۸. فعال‌سازی RoutingService (پیش‌نیاز استقرار)

1. `web/service/config.json`: آرایه‌ی `api.services` → افزودن `"RoutingService"`.
2. کانفیگ در حال اجرا (DB / فایل bin/config.json روی سرور): همان افزودنی — یا از طریق Xray Settings پنل (اگر بخش API قابل ویرایش است) یا migration کوچک در `xray_setting.go` که هنگام ذخیره/بارگذاری کانفیگ، اگر `RoutingService` نبود اضافه کند + restart Xray.
3. بعد از restart: تست سلامت — فراخوانی `AddRule`/`RemoveRule` با یک قانون بی‌اثر (مثلاً user=`__rafael_healthcheck__`) و انتظار موفقیت؛ نتیجه در لاگ موتور.

> ترتیب استقرار ایمن: اول بیلد با تغییرات پنل (که RoutingService را اضافه و fallback را دارد) → آپدیت سرور → restart → موتور خودش RoutingService را تشخیص می‌دهد و sticky blocking فعال می‌شود. اگر کانفیگ برای هر دلیل migrate نشد، fallback (بخش ۶) فعال است.

---

## ۹. پلان تست

**Unit (web/service — الگوی client_limit_engine_test.go موجود):**

1. `TestLimitRouterEnforceBuildsRule` — mock xrayAPI؛ تأیید: فراخوانی AddRule با tag درست، `ShouldAppend=true`، JSON حاوی user/source/32/blocked.
2. `TestLimitRouterReleaseRemovesRule` — Release → RemoveRule با همان tag.
3. `TestLimitRouterEnforceIdempotent` — دو بار Enforce برای همان (email,ip) → فقط یک جفت Add/Remove (دومین Add قبل‌تر RemoveRule همان tag می‌فرستد — تأیید ترتیب).
4. `TestLimitRouterFallbackOnApiError` — mock خطا → engine به kick-only برمی‌گردد، خطا fatal نیست، retry در تیک بعد.
5. `TestEngineInstallsRuleOnViolation` (integration engine+router با mock xray) — سناریوی walkthrough بخش ۴.۴: دو IP، لیمیت ۱، پس از grace: Enforce یک‌بار + کیک؛ اتصال جدیدِ IP خاطی دیگر accept نمی‌شود (در mock: assert روی AddRule).
6. `TestEngineReleasesRuleWhenUnderLimit` — IP خاطی TTL می‌گذرد → Release فراخوانی می‌شود.
7. `TestEngineReleaseAllOnClientDisable` — disable کلاینت → ReleaseAll.
8. تست‌های موجود ۱۹گانه engine باید **بدون تغییر** پاس بمانند (رفتار کیک پیش‌فرض حفظ شده) — جز آن‌هایی که assert تعداد کیک دارند که با Enforce جدید یکی می‌مانند (کیک همچنان ۱).

**E2E دستی روی سرور تست (پروتکل):**

```
1. آپدیت بیلد → تأیید لاگ «RoutingService available»
2. کلاینت تست limitIp=1, Grace=10
3. دو اینترنت؛ IP دوم ترافیک مستمر
4. انتظار:
   - [LIMIT_IP] در 3xipl.log (موجود)
   - [LIMIT_RULE] در 3xipl.log (جدید — نصب قانون)
   - اتصال جدید از IP قدیمی: به‌صورت فوری رد می‌شود (کلاینت connect error)
   - IP جدید بدون وقفه کار می‌کند
5. قطع IP دوم، اتصال از IP اول: با انقضای TTL، Release → دوباره وصل می‌شود
```

---

## ۱۰. نظارت

- لاگ: هر Enforce/Release/Fallback با prefix `[LIMIT_RULE]` در همان مسیر لاگ موتور (`/var/log/x-ui/3xipl.log`).
- endpoint پنل (اختیاری، فاز ۲): `GET /panel/api/inbounds/limitRules` → `Snapshot()` برای نمایش قانون‌های فعال در UI.

---

## ۱۱. محدودیت‌های شناخته‌شده (صادقانه)

1. **اولین اتصال هر IP خاطی همیشه برای ~۱ تیک + grace موفق است** — ریشه در معماری VLESS (اطلاعات email فقط بعد از رمزگشایی payload وجود دارد). حذف این پنجره فقط با fork کردن Xray ممکن است — خارج از حد این طراحی.
2. **قوانین داینامیک کم‌اولویت‌اند** (append به انتها) — قوانین سفارشیِ جلوترِ اپراتور برای مقصدهای خاص می‌توانند قبل از قانون ما مچ شوند (بخش ۲-ب).
3. **قوانین در restart Xray پاک می‌شوند** (in-memory) — بازسازی خودکار در تیک بعد؛ پنجره‌ی کوتاه.
4. **`user_email` match بعد از احراز هویت در inbound اتفاق می‌افتد** — یعنی درِ واقعاً «درِ احراز هویت» است: کانکشن TCP برقرار می‌شود ولی هیچ بایتی پروکسی نمی‌شود. این دقیقاً همان چیزی است که کاربر خواست («درخواست اتصال جدید که آمد رد بشه»).
5.CGNAT: دو کاربر واقعی پشت یک CGNAT IP عمومی با یک اینباند — policy با email جدا است و قانون per-(email, ip) است؛ interference نمی‌کند چون user-email هم باید مچ شود.

---

## ۱۲. ترتیب پیاده‌سازی (چک‌لیست)

- [x] ۱. `xray/api.go`: افزودن `RoutingServiceClient` + `AddRoutingRule` + `RemoveRoutingRule` (+ unit tests با mock interface)
- [x] ۲. `web/service/config.json`: افزودن `"RoutingService"` به `api.services`
- [x] ۳. `database/db.go`: migration — `ensureRoutingServiceInTemplate()` داخل `normalizeLegacyData` + تست `db_template_test.go` (legacy/بدون api/JSON خراب/نبود row)
- [x] ۴. فایل جدید `web/service/client_limit_router.go` + تست‌های ۱-۴ بخش ۹
- [x] ۵. `client_limit_engine.go`: نقاط Enforce/Release/ReleaseAll در `maintainOnce` + قلاب‌های notify + fallback + comparator مشترک `sightingNewer` (کشف و)
- [x] ۶. تست‌های ۵-۸ بخش ۹ (هر ۸ تست ۳ بار PASS)؛ کل سوئیت `web/service` PASS + `go build ./...` + `go vet` OK + تست migration دیتابیس PASS
- [x] ۷. بیلد آرشیو v39 — SHA-256 `ef50b14a…c30095`، پنل `c903b49d…d10e76`؛ سوئیت آپدیتر ۳۰۴/۳۰۴ + شبیه‌سازی v37 (۷۷/۷۷) PASS
- [ ] ۸. استقرار روی سرور تست + پروتکل E2E بخش ۹

---
---

# فاز ۲ — گیت احراز درون Xray (fork) : حذف کامل پنجره‌ی زمانی

> **هدف فاز ۲:** اولین اتصالِ هر IP اضافه، **در همان لحظه‌ی احراز هویت** رد شود — بدون grace، بدون انتظار برای لاگ. فاز ۱ این را برای اتصال‌های *بعدیِ* IPِ شناخته‌شده حل کرد؛ فاز ۲ پنجره‌ی ~۱۰ ثانیه‌ایِ اولین اتصال هر IP جدید را هم می‌بندد.
> **راه‌حل:** fork کردن Xray-core و افزودن یک هوک شمارنده‌ی همزمان (in-process) در نقطه‌ی همگرایی همه‌ی پروتکل‌ها.

## ۱۳. چرا fork و چرا حالا مسئله‌ی «دیتابیس» نیست

(خلاصه‌ی بحث جلسه:) برای تصمیم لحظه‌ای دو چیز لازم است:

1. **دانستن وضعیت** — «این ایمیل چند IP فعال دارد؟» ← از قبل در حافظه‌ی موتور داریم؛ دیتابیس چیزی اضافه نمی‌کند.
2. **فرصتِ پرسیدن** — نقطه‌ای که Xray *قبل از پذیرش* بپرسد ← **این در Xray رسمی وجود ندارد** و تنها راه ساختنش fork است.

اما نکته‌ی کلیدی که fork را برای *ما* ارزان می‌کند: **پنل رافائل از قبل Xray اختصاصی خودش را در آرشیو توزیع می‌کند** (`rafael/bin/xray-linux-amd64`). کاربر هیچ‌وقت Xray رسمی نصب نمی‌کند؛ آپدیتر ما باینری را می‌گذارد. پس هزینه‌ی واقعی fork فقط **نگهداری merge با upstream** است، نه توزیع.

## ۱۴. یافته‌های سورس — نقطه‌ی هوک (همه روی v1.260327.0 قفل‌شده تأیید شده)

### ۱۴.۱. استخراج کاربر در هر پروتکل (قبل از رله)

| پروتکل | نقطه‌ی دقیق | نحوه |
|---|---|---|
| VLESS | `proxy/vless/encoding/encoding.go:94` | `request.User = validator.Get(id)` داخل `DecodeRequestHeader` |
| Trojan | `proxy/trojan/server.go:188` | `user = s.validator.Get(hexString(first.BytesTo(56)))` |
| VMess | `proxy/vmess/inbound/inbound.go:150` + `:273` | `usersByEmail.GetOrGenerate(email)` → `inbound.User = request.User` |
| Shadowsocks | `proxy/shadowsocks/server.go:154` + `validator.go:112` | `inbound.User = request.User` بعد از `validator.Get(bs, command)` |

### ۱۴.۲. ⭐ نقطه‌ی همگرایی — `DefaultDispatcher.routedDispatch` (کشف کلیدی فاز ۲)

هر چهار پروتکل **قبل از** فراخوانی `dispatcher.Dispatch(...)` مقدار `session.Inbound.User` و `session.Inbound.Source` را در context ست می‌کنند (VLESS در `inbound.go:537`، Trojan در `server.go:228`، VMess در `inbound.go:273`، SS در `server.go:154`). یعنی:

هوک در ابتدای `routedDispatch` قرار دارد؛ این تابع از مسیرهای `Dispatch` و `DispatchLink` فراخوانی می‌شود.

**یک هوک در ابتدای همین متد، همه‌ی پروتکل‌ها را پوشش می‌دهد** — نیازی به ۴ هوک جدا در کد هر پروتکل نیست. در این نقطه:
- `session.InboundFromContext(ctx).User.Email` → ایمیل کلاینت (تأیید شده)
- `session.InboundFromContext(ctx).Source` → IP:port مبدا (تأیید شده)
- هیچ بایتی از داده‌ی کاربر هنوز رله نشده ← رد در اینجا = همان «گیت احراز» مطلوب

### ۱۴.۳. نکته‌ی معماری: چرا شمارنده باید *درون Xray* باشد

اگر هوک فقط از پنل بپرسد («این ایمیل پر است؟»)، جواب پنل از state لاگ‌محور می‌آید که ~۱ ثانیه تأخیر دارد. برای **صفرِ واقعی**، شمارش باید همزمان با خود اتصال انجام شود:

- **افزایش:** هوک در `Dispatch` — اگر `(email, sourceIP)` تازه است و تعداد IPهای فعالِ آن ایمیل ≥ `limitIp` → رد فوری.
- **کاهش:** پایان همان connection (هر پروتکل در `Process` خودش defer دارد؛ هوک کاهش را با `ctx` همان اتصال در closure رله می‌کند — یا ساده‌تر: ثبتِ `net.Conn` بسته‌شدنی در tracker).

پس fork یک **IP-activity tracker درون‌حافظه‌ای** دارد که دقیقاً همزمان با اتصال‌ها به‌روز می‌شود — نه از لاگ، نه از پنل. پنل فقط **مقدار limit هر ایمیل** را به آن تحویل می‌دهد (بخش ۱۶).

## ۱۵. استراتژی fork — «افزودنی‌محور» (additive-only)

قاعده‌ی طلایی برای بقا در برابر upstream:

1. **همه‌ی منطق جدید در فایل‌های جدید** (مثلاً `app/iplimit/tracker.go`، `app/iplimit/service.go`) — فایل جدید در merge هرگز conflict نمی‌دهد.
2. **تغییر کد موجود فقط به‌صورت درج‌های چندخطی** با نشانگر:
   ```go
   // RAFAEL:IPLIMIT {
   if err := iplimit.Check(ctx); err != nil { return nil, err }
   // RAFAEL:IPLIMIT }
   ```
   نقطه‌ی فعال گیت ابتدای `DefaultDispatcher.routedDispatch` است؛ release با `context.AfterFunc` به پایان context نشست متصل است.
3. **هیچ خطی از کد upstream حذف یا تغییر داده نمی‌شود.**
4. **Repo:** `rafael-xray-core` — clone از `XTLS/Xray-core` در شاخه‌ی `rafael`؛ upstream به‌صورت remote دوم؛ upgrade = merge دوره‌ای به انتخاب ما (نه هر release).
5. **پروتکل کنترل در فایل proto جدید** (`app/iplimit/command/command.proto` + کد تولیدی) — پیام‌های upstream دست نمی‌خورند.

## ۱۶. پروتکل همگام‌سازی پنل ↔ fork (طرح اولیه؛ در Build 41 با فایل سیاست جایگزین شده)

**سرویس gRPC جدید درون Xray** (فقط روی dokodemo/tunnel API داخلی `127.0.0.1` — همان مسیر HandlerService فعلی):

```proto
service IPLimitControlService {
    rpc SetIPLimit(SetIPLimitRequest) returns (SetIPLimitResponse);
    rpc RemoveIPLimit(RemoveIPLimitRequest) returns (RemoveIPLimitResponse);
    rpc GetIPLimitStatus(GetIPLimitStatusRequest) returns (GetIPLimitStatusResponse); // برای UI
}
message SetIPLimitRequest { string email = 1; int32 limit_ip = 2; int64 grace_ms = 3; }
```

- **پنل کی صدا می‌زند:** درست همان قلاب‌هایی که امروز `clientLimitEngineNotifyClientUpdated` را صدا می‌زنند (ذخیره/غیرفعال‌سازی/حذف کلاینت در `inbound.go`) — به‌علاوه در startup برای همه‌ی کلاینت‌های دارای limitIp (bootstrap یک‌باره).
- **پروتکل رد:** هوک همان رفتار «invalid user» استاندارد پروتکل را برمی‌گرداند (نه connection reset عجیب) تا کلاینت‌ها رفتار retry عادی داشته باشند و اثر انگشت فیلترچی افزایش نیابد.
- **رفتار طرح اولیه:** fail-open بود. در Build 41، فایل سیاست strict است: snapshot ناموجود در شروع و ایمیل ناشناخته رد می‌شوند؛ snapshot خراب پس از بارگذاری، آخرین نسخه‌ی سالم را حفظ می‌کند.

## ۱۷. تعامل با فاز ۱ (طرح اولیه؛ در Build 41 موتور قدیمی فقط نمایش می‌دهد)

| نگرانی | نقش فاز ۲ (fork) | نقش فاز ۱ (موجود) |
|---|---|---|
| اتصال جدید IP اضافه | ⭐ رد در همان لحظه (صفر ثانیه) | — |
| کانکشن establishedِ متخلف | — | کیک (RemoveUser/AddUser) همان‌طور که هست |
| اجرا با Xray رسمی (بدون fork) | — | fallback کامل: تشخیص لاگی + قوانین چسبنده |
| UI/مشاهده‌ی IPهای فعال | — | state موتور + `Snapshot()` |
| restart Xray | شمارنده‌ها صفر؛ با reconnect طبیعی بازسازی (همه reconnect می‌زنند) | قوانین با تیک بعدی re-arm |

این جدول رفتار تاریخی را شرح می‌دهد. در Build 41 موتور لاگ grace/kick و router آن در مسیر تولید غیرفعال‌اند؛ گیت Xray تنها مرجع پذیرش اتصال است.

**قاعده‌ی Build 41:** کلاینت‌های نامحدود ورودی صریح با `limitIp=0` دارند؛ ایمیل ناشناخته در snapshot سخت‌گیرانه رد می‌شود. کلاینت غیرفعال هم اتصال جدید نمی‌گیرد.

## ۱۸. مسیر build و توزیع

1. fork در `forks/xray-core/` (submodule یا clone داخل monorepo رافائل؛ تصمیم پیاده‌سازی).
2. build باینری `xray-linux-amd64` از fork (همان toolchain CGO_ENABLED=0).
3. آرشیو آپدیتر: فقط محتوای `rafael/bin/xray-linux-amd64` عوض می‌شود — **updater و panel بدون تغییر** (ساختار آرشیو ثابت).
4. **rollback بدون دردسر:** اگر باینری fork مشکل داشت، گذاشتن باینری رسمی در آرشیو بعدی کافی است — پنل با نبودِ `IPLimitControlService` به حالت فاز ۱ برمی‌گردد (همان مکانیزم fallback موجود). این هم‌راستا با تگ `v39-sticky-iplimit` به‌عنوان خط بازگشت.

## ۱۹. پلان تست فاز ۲

**Unit (درون fork):**
1. `TestTrackerAllowsWithinLimit` — limit=1، اولین IP اجازه.
2. `TestTrackerRejectsSecondIP` — limit=1، IP دومِ *متفاوت* رد؛ IP اول ادامه.
3. `TestTrackerReleasesOnConnectionClose` — بسته‌شدن اتصال → IP آزاد → IP جدید اجازه.
4. `TestTrackerSameIPReconnect` — reconnect همان IP تکرار شمرده نمی‌شود.
5. `TestTrackerFailOpen` — ایمیل بدون limit → همیشه اجازه.
6. `TestTrackerConcurrency` — ۱۰۰۰ اتصال همزمان؛ بدون data race (`go test -race`).

**Integration (پنل + fork، روی VM لینوکس):**
7. پنل limitIp=1 ست می‌کند → gRPC → دو اتصال از دو IP → دومین باید **در همان handshake** رد شود (تأخیر < 100ms؛ بدون هیچ خط `[LIMIT_IP]`/`[LIMIT_RULE]` لازم).
8. غیرفعال‌سازی کلاینت → `RemoveIPLimit` → اتصال مجدد آزاد.
9. جایگزینی باینری با Xray رسمی → رفتار فاز ۱ (fallback) — regression test خودکار سناریوی امروز.

**E2E (سرور تست):** همان پروتکل بخش ۹ با انتظار جدید: **صفر** پنجره‌ی آزاد برای IP دوم.

## ۲۰. ریسک‌ها و هزینه‌ها (صادقانه)

1. **بار نگهداری merge:** هر upgrade آینده‌ی Xray = merge شاخه‌ی ما. با استراتژی افزودنی‌محور (بخش ۱۵) به چند conflict کوچک محدود می‌شود، ولی صفر نیست. **ارزشش را فقط دارد اگر پنجره‌ی ۱۰ ثانیه‌ای عملاً به فروش/امنیت لطمه بزند** — تصمیم پس از دوره‌ی واقعی فاز ۱ روی سرور.
2. **Delay هوک در مسیر اتصال:** چک درون‌حافظه‌ای است (RWMutex + map) — نانوثانیه‌ای؛ ولی باید benchmark شود که در p99 هیچ افزودگی محسوس ندارد.
3. **رفتار UDP:** UDP «اتصال» ندارد؛ tracker باید sessionهای UDP را با timeout به‌عنوان اتصال فرضی بشمارد (همان منطق TTL فاز ۱، اما درون Xray). در پیاده‌سازی باید دقیق شود.
4. **Mux:** چند استریم روی یک connection — شمارش per-connection کافی است چون limit بر IP است نه استریم.
5. **اثر انگشت:** رفتار ردِ استاندارد استفاده می‌شود (۱۴/۱۶) ولی یک ناظر فعال (active prober) می‌تواند تفاوت ظریف بیابد — قابل قبول.
6. **Licensing:** Xray تحت MIT — fork و توزیع مجدد آزاد است؛ حفظ copyright headerها الزامی.

## ۲۱. ترتیب پیاده‌سازی فاز ۲ (چک‌لیست)

- [x] ۱. fork در `forks/xray-core` از سورس قفل‌شده‌ی v1.260327.0 (کپی از module cache) — شاخه‌ی `rafael`، کامیت پایه `4efcb96` (دست‌نخورده) + کامیت فاز ۲ `7801679`؛ build پایه تأیید شد
- [x] ۲. `app/iplimit/tracker.go` — شمارنده‌ی همزمان + تست‌های ۱-۶ (۸ تست PASS ×۲؛ -race روی cross-compile ممکن نیست، race-safety با mutex + تست concurrency)
- [x] ۳. کانال کنترل: **فایل JSON به‌جای proto** (protoc در دسترس نبود و فایل از proto در برابر merge مقاوم‌تر است) — پنل `iplimits.json` را در DB folder می‌نویسد، fork با mtime-watch (تیکر ۱۰ثانیه + خواندن startup) دنبالش می‌کند؛ env var `XRAY_IPLIMIT_FILE`
- [x] ۴. درج هوک در `routedDispatch` (کشف §۱۴.۲: VLESS از DispatchLink می‌رود؛ نقطه‌ی همگرای واقعی هر دو مسیر routedDispatch است — درج ۹خطی با نشانگر RAFAEL:IPLIMIT؛ release با defer)
- [x] ۵. سمت پنل: `web/service/client_limit_gate.go` (RefreshFromDB؛ no-op بدون بازنویسی؛ محکم‌ترین limit برای emailهای تکراری) + `xray/process.go` env var هنگام spawn + `GetIPLimitConfigPath` + قلاب startup و `notifyClientUpdated` (defer؛ حتی وقتی engine بالا نیست)
- [x] ۶. تست‌های سمت پنل: ۴ تست gate PASS؛ کل تست‌های IP-limit ۳۵/۳۵ PASS (تست custom_geo شبکه‌ای مستقلاً به‌دلیل قطعی DNS محیط fail — بی‌ربط به تغییرات)
- [x] ۷. build آرشیو v40 در **build-v40** (build-v39 دست‌نخورده): marker v40، پنل `587742d1…`، xray fork `64703bbd…`، آرشیو `24376d70…` (بازبیلد بعد از اصلاحات نصب فلت — زیر)، pin آپدیتر `1a31acf4…`
- [x] ۷ب. **نصب فلت (دستور دستی اپراتور):** آرشیو باید با این دستور هم بوت شود:
  `tar -xzf … && cp -f rafael/rafael.service.debian /etc/systemd/system/rafael.service && chmod +x … && cp rafael/rafael.sh /usr/bin/rafael && mv rafael/ /usr/local/rafael/ && systemctl enable --now rafael`
  سه ایراد واقعی پیدا و رفع شد:
  1. **chdir failure** — systemd قبل از ExecStartPre به WorkingDirectory می‌رود؛ لینک current هنوز ساخته نشده. رفع: خط دوم `WorkingDirectory=-/usr/local/rafael/current` (systemd آخرین assignment را برمی‌دارد؛ خط اول عمداً دست‌نخورده ماند چون validator آپدیتر مستقر v39 آن را verbatim چک می‌کند). symlink داخل tar ممکن نیست — `check_no_special_members` آپدیتر عضو symlink را رد می‌کند (سخت‌گیری ضد symlink-attack، درست).
  2. **تله‌ی first-boot env** — systemd فایل EnvironmentFile را قبل از اجرای bootstrap.sh می‌خواند؛ در اولین بوتِ فلت فایل هنوز وجود ندارد (پرچم `-` نادیده‌اش می‌گیرد) → پنل بدون XUI_DB_DSN با sqlite بالا می‌آمد و بعد از restart به postgres خالی سوییچ می‌کرد. رفع: `loadRafaelEnv()` در main.go فایل /etc/default/rafael را با godotenv بار می‌کند (هرگز متغیر موجود را override نمی‌کند → سیستم‌های release-layout بی‌تأثیر). تست: main_env_test.go PASS.
  3. **تشخیص آپدیتر** — بعد از اولین بوتِ فلت، لینک self-pointing `current → .` ساخته می‌شد و `is_flat_install()` (که `! -e current` می‌خواست) نصب را release می‌پنداشت. رفع: `is_flat_install` حالا «current که به release resolve نمی‌شود» را هم فلت می‌داند؛ `resolve_current` هم لینک self-pointing را برمی‌گرداند empty (نه نام نامعتبر «rafael»). تست جدید T3b در سوئیت آپدیتر همین مسیر (فلت بوت‌شده → migration تمیز) را می‌پوشاند.
  نکته‌ی دستور اپراتور: `mv rafael/ /usr/local/rafael/` بعد از `cp rafael.sh /usr/bin/rafael` — یعنی CLI کپی‌شده همان rafael.sh رهاسازی است و `rafael update <archive>` از همان اول کار می‌کند (bootstrap bridge با pin داخل خودش).
- [ ] ۸. استقرار سرور تست + E2E: انتظار جدید = **صفر پنجره‌ی آزاد** برای IP دوم (رد در همان handshake، بدون هیچ خط [LIMIT_IP] لازم)
- [ ] ۹. تصمیم go/no-go برای production (معیار: پنجره‌ی صفر + صفر regression نسبت به فاز ۱)

### نکات کشف‌شده حین پیاده‌سازی فاز ۲

- **پروکسی Go از این شبکه قطع است** (proxy.golang.org → 403/000)؛ بیلدها با mirror `goproxy.cn` انجام شد (`GOPROXY=https://goproxy.cn,direct`).
- **موقعیت درج هوک:** طراحی اولیه `Dispatch` را نقطه‌ی همگرا می‌دانست؛ تحقق در سورس نشان داد VLESS از `DispatchLink` می‌رود و مسیر sniffing هم `routedDispatch` را در goroutine صدا می‌زند — پس درج در ابتدای `routedDispatch` است که هر ۴ نقطه‌ی فراخوانی (خطوط 302/331/358/386) را پوشش می‌دهد.
- **الگوی release:** release با `context.AfterFunc` به پایان context نشست ورودی متصل است تا UDP و multiplex با پایان یک جریان مقصد، slot را زود آزاد نکنند.
- **معناشناسی Build 41:** پس از پایان نشست، سهمیه برای IP دیگر فوراً آزاد می‌شود. slot بی‌اتصال که مدتی در حافظه می‌ماند seat فعال نیست و IP دیگر را منتظر نمی‌گذارد.
- **UDP:** slot در عمر context نشست ورودی UDP نگه داشته می‌شود، حتی اگر بین بسته‌ها جریان مقصدی فعال نباشد.

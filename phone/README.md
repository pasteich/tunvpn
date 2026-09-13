# TunVPN — Android-обёртка для туннеля notes.mail.ru

Полноценное Android-приложение (VPN), которое заворачивает **весь трафик телефона**
через твой туннель (`../main.go`), использующий notes.mail.ru как транспорт.

## Как это устроено

```
Приложения телефона
      │  весь трафик (IPv4+IPv6)
      ▼
 VpnService (TUN 10.0.0.2 / fd00::2)
      │  файловый дескриптор TUN
      ▼
 tun2socks (встроен в приложение через gomobile AAR)
      │  SOCKS5 CONNECT (TCP)  +  DNS→TCP (порт 53)
      ▼
 libtun.so  = твой проект (-client), SOCKS5 на 127.0.0.1:8888
      │  WebSocket
      ▼
 notes.mail.ru  ──►  сервер (твой -server где-то на выходе)  ──►  интернет
```

- **libtun.so** — это ровно твой основной проект, собранный как бинарник
  (`GOOS=android`, **CGO включён** через NDK — иначе не работает DNS на Android).
  Запускается приложением как дочерний процесс.
- **tun2socks** ([xjasonlyu/tun2socks](https://github.com/xjasonlyu/tun2socks))
  встроен в процесс приложения (gomobile). TUN-дескриптор передаётся ему напрямую
  числом — без ненадёжной передачи fd между процессами.
- Трафик самого приложения (WebSocket к notes.mail.ru) **исключён из VPN**
  (`addDisallowedApplication`), поэтому петли нет.

### Важное ограничение: SOCKS в туннеле — только TCP
В `main.go` SOCKS5 поддерживает только `CONNECT` (UDP ASSOCIATE отклоняется).
Поэтому:
- **TCP** (сайты, приложения) — работает полностью.
- **DNS** — работает: обёртка перехватывает UDP:53 и гонит запросы
  как **DNS-over-TCP** через туннель (без DNS-leak). См. `t2s.go`.
- **Прочий UDP** (QUIC/HTTP-3, игры по UDP) — не проходит; приложения
  сами откатываются на TCP. Чтобы включить полноценный UDP, нужно добавить
  UDP ASSOCIATE в серверную часть туннеля.

## Требования на второй стороне
Приложение — это **клиент**. Чтобы был интернет, нужно:
1. Действующие **`-pipe`** и **`-token`** (сессия notes.mail.ru).
   Получить их: userscript [`../tampermonkey.txt`](../tampermonkey.txt) для
   расширения Tampermonkey (удобно, ставится один раз) либо сниппет
   [`../cred.txt`](../cred.txt) в консоли DevTools. Инструкции — в шапке файлов.
2. Тот же бинарник в режиме **`-server`**, запущенный на выходной машине
   с тем же `-pipe`/`-token`. Он и дозванивается до реальных адресов.

## Использование
1. Открой **TunVPN** на телефоне.
2. Введи **Pipe key** и **Token** (режим оставь `awareness`, либо `sync` — быстрее).
   Все крутилки — под «Advanced settings» (значения по умолчанию совпадают с флагами `main.go`).
3. Нажми **Start VPN** → разреши системный запрос VPN.
   Статус станет «Connected», в шторке появится значок VPN.
4. Остановка — кнопкой **Stop VPN** (всегда внизу) или **Stop** в уведомлении.
5. Все настройки сохраняются между запусками.

## Сборка

Тулчейн ставится локально (без root): JDK 17, Android SDK (platform-34,
build-tools 34), NDK r26d, Gradle 8.7, gomobile.

### 1. Пересобрать бинарник туннеля (`libtun.so`) из `../`:
```bash
NDK=$ANDROID_HOME/ndk/26.3.11579264
CC=$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android24-clang
cd ..                       # каталог с main.go
CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC="$CC" \
  go build -trimpath -ldflags "-s -w" \
  -o phone/app/src/main/jniLibs/arm64-v8a/libtun.so .
```
> CGO обязателен: с `CGO_ENABLED=0` Go не видит DNS-серверы Android
> (падает с `lookup ... on [::1]:53: connection refused`).

### 2. Пересобрать tun2socks-обёртку (`t2smobile.aar`), если менял `t2s.go`:
```bash
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/26.3.11579264
cd ~/android-dev/t2smobile
gomobile bind -target=android/arm64 -androidapi 24 -ldflags="-s -w" -o t2smobile.aar .
cp t2smobile.aar /path/to/phone/app/libs/
```

### 3. Собрать APK:
```bash
cd phone
gradle :app:assembleDebug
# → app/build/outputs/apk/debug/app-debug.apk
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

## Структура проекта
```
phone/
├── app/
│   ├── libs/t2smobile.aar                     # tun2socks (gomobile), собирается из ~/android-dev/t2smobile
│   ├── src/main/
│   │   ├── AndroidManifest.xml
│   │   ├── jniLibs/arm64-v8a/libtun.so        # твой туннель как бинарник
│   │   ├── java/com/tun/vpn/
│   │   │   ├── MainActivity.java              # UI: поля, крутилки, статус, лог
│   │   │   ├── TunVpnService.java             # VpnService: TUN + запуск бинарника + tun2socks
│   │   │   ├── Config.java                    # все настройки + сохранение (SharedPreferences)
│   │   │   └── TunState.java                  # статус/лог-шина между сервисом и UI
│   │   └── res/…                              # разметка, тема, иконка
│   └── build.gradle
├── build.gradle · settings.gradle · gradle.properties · local.properties
└── TunVPN.apk                                 # готовый APK
```

Целевое устройство протестировано: Redmi, Android 14 (arm64-v8a).
Только arm64-v8a. Для armeabi-v7a/x86 нужно собрать `libtun.so` и AAR под эти ABI.

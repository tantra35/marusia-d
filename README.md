# marusia-d

Демон периферии колонки VK Mini («Маруся», Amlogic A113X, mainline 6.18 + Alpine/OpenRC):
состояние MPD + сенсорная панель → световое кольцо, панель-часы, автояркость.

## Периферия и интерфейсы (из device tree + stock-прошивки)

| Что | Железо | Как управляется |
|---|---|---|
| Кольцо | 12× WS2812-подобных, SPI0 | `/dev/spidev0.0`, 3.35 МГц, 4 SPI-бита на бит WS2812 (0→1000, 1→1110), GRB, reset 128 байт ≈300 мкс |
| Сенсоры | Hynitron CSK05T, I2C @0x15 | evdev `/dev/input/event0`, keycodes 114, 115, 248, 148, 582 (из DT keys@15) |
| Панель-часы | IS31FL3236A, I2C @0x3C | seg-led класс: `linedisp.*/message`, `map_seg7` (маски через коды 0x01–0x04), colon = leds, яркость на родителе, `current_divider` 1–4 |
| Датчик света | Rohm BH1730, I2C @0x29 | IIO `in_illuminance_input` → множитель яркости кольца 0.15–1.0 |

## Логика

- MPD `player`: `play` → радуга, `pause` → дыхание (янтарь), `stop` → тьма.
- Панель-часы: время ЧЧ:ММ, при play — лицо wow, при pause — sleepy, потом снова часы.
- Сенсоры: 115 → vol+5, 114 → vol−5 (фидбек — белая заливка кольца на 600 мс),
  248 → play/pause (предположение — сверить `touch-keys` на устройстве!), прочие коды — в журнал.
- Датчик света: EMA по lux → глобальный масштаб яркости кольца.

## Связь с LVA (linux-voice-assistant)

Роли перевёрнуты относительно upstream: **слушает колонка** (`:6055`),
LVA подключается к ней сам. Колонка не знает адреса LVA и молча работает
изолированно, пока мозг не поднят; на стороне LVA нужен патч (клиент ws
к периферии вместо сервера). Все адреса — в одном месте, в конфиге LVA:

    LVA → ws://<ip-колонки>:6055

Включено по умолчанию; `marusia-d -lva ""` — полностью выключить.
«Последний побеждает»: повторный коннект LVA выкидывает прежнего клиента.

События → сцены кольца:

| Событие LVA | Сцена |
|---|---|
| `wake_word_detected` | wake — две зелёные вспышки |
| `listening` | listening — синяя комета по кольцу |
| `thinking` | thinking — фиолетовая вращающаяся дуга |
| `tts_speaking` | speaking — голубой пульс |
| `tts_finished` / `idle` | возврат к сцене MPD |
| `snapshot` | muted (сцена) + `volume` 0.0–1.0 — синхронизация громкости sink при коннекте |
| `muted true` | muted — тускло-красный статичный |
| `pipeline_error` | error — три красные вспышки |
| обрыв связи | noconn — тёмно-красное дыхание (после первого удачного коннекта) |
| `media_player_playing` | радуга (музыка через LVA) |
| `volume_changed` | `{"volume": 0.0–1.0}` (upstream) → громкость системного sink (sink = volume×150%) + вспышка цветом уровня и звук |

Приоритет сцен: `solid (HA) > error > noconn > muted > диалог > MPD`.

Обратно (сенсорные кнопки): 114/115 → при подключённом LVA — upstream-команды
`volume_up`/`volume_down` (LVA меняет громкость, шлёт `volume_changed`, sink
подтягивается; значение живёт в LVA/HA), автономно — локально `pactl` на sink.
Громкость DLNA (слайдер контроллера) — через хуки upmpdcli
(`externalvolumecontrol` → `/usr/local/bin/upmpdcli-volhook` → TCP-порт 6056
демона, флаг `-volapi`): master = системный sink PA, mpd-поток громкость не
пишет (`mixer_type "none"` в mpd.conf). Визуальная шкала (кольцо/панель) —
0..100 при физических 0..150% sink.
582 → `start_listening`, 148 → play/pause (mpd), 248 → `mute_mic`/`unmute_mic`
(сверить keycode'и на устройстве!). С `-lva-light` (по умолчанию) кольцо
регистрируется как light-сущность HA (`light.<satellite>_ring`): вкл/выкл, яркость
и RGB из Home Assistant управляют кольцом напрямую (сцена `solid`), а эффект
«Voice Assistant» возвращает управление демону.

MPD и LVA не фатальны: оба линка переподключаются сами, кольцо честно показывает
состояние (`noconn`) только после первой удачной связи с LVA.

## Сборка

Go лежит в userspace: `/opt/go-1.26.10/bin`.

```bash
export PATH=/opt/go-1.26.10/bin:$PATH
cd ~/Загрузки/marusia/marusia-d
go test ./...          # энкодер кольца
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
    -o ../build-rootfs/template/usr/local/bin/marusia-d .
```

Статический бинарник ~2.5 МБ, без зависимостей — попадает в образ через template
при очередной сборке rootfs (`build-rootfs.sh`), init-скрипт уже в
`template/etc/init.d/marusia-d` (after mpd pulseaudio).

## Отладка на устройстве

```sh
# фидбек сенсорной панели (сверить keycode'и!)
touch-keys
# эталон кадра vendor-утилиты против нашего:
ringled --dump test
marusia-d -ring-dump ffffff
# вручную:
marusia-d -mpd 127.0.0.1:6600            # все флаги опциональны
```

## TODO (по факту на устройстве)

- сверить кодирование кадра: `ringled --dump` vs `marusia-d -ring-dump` (байт в байт);
- keycode 582 — что это (сейчас: 582=start_listening, 148=play/pause, 248=mic mute);
- часовой пояс панели-часов (Alpine может жить в UTC);
- яркость панели-часов от BH1730: PWM 12–255 + current_divider 1–4 (реализовано: `light.ClockPWM`/`ClockCurrent`);
- коллизия с ringled: демон и vendor-утилита не должны держать spidev одновременно.

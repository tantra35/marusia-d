# Отладочные утилиты MPD-прокси / DLNA

- `dlna-ctl.py` — контроллер DLNA с ноута: SSDP unicast-поиск рендерера,
  SOAP RenderingControl/AVTransport (getvol/setvol/seturi/play/pause/stop).
  Запуск: `python3 dlna-ctl.py <getvol|setvol N|seturi URL|play|pause|stop>`.
- `rc-subscribe.py` — подписка на RenderingControl+AVTransport NOTIFY
  (LastChange: TransportState/Volume/Mute) с callback-сервером на :8899.
  Запуск: `python3 rc-subscribe.py <секунд>`; предупреждает, если upmpdcli
  рестартовали (подписка умирает — перезапустить скрипт).
- `proxy-accept/main.go` — приёмка прокси НА ПЛАТЕ (dropbear не даёт
  ssh -L): подключается к 127.0.0.1:6600 локально, проверяет setvol/
  status-вставку/idle/нотификацию/mute, sink проверяет через pactl.
  Сборка: `CGO_ENABLED=0 GOARCH=arm64 go build -o proxy-accept .`,
  запуск на колонке: `/tmp/proxy-accept`.

Требования: python3 на ноуте (без внешних зависимостей), ssh на плату.

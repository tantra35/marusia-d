//! Порт internal/lva (Go): серверная сторона Peripheral WebSocket API Linux
//! Voice Assistant (docs/peripheral_api.md). Роли перевёрнуты относительно
//! upstream: marusia-d слушает (по умолчанию :6055), LVA подключается сам —
//! колонка не знает адреса LVA и молча работает изолированно, пока мозг не
//! поднялся. Слушает события диалога (wake/listening/thinking/speaking/idle...)
//! и шлёт команды (start_listening, mute_mic...).
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use futures_util::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio::sync::watch;
use tokio_tungstenite::tungstenite::Message;

use crate::{logln, util};

/// Сообщение LVA: {"event": "...", "data": {...}}.
#[derive(Clone, Debug)]
pub struct Event {
    pub event: String,
    pub data: Value,
}

impl Event {
    pub fn bool(&self, key: &str) -> bool {
        self.data.get(key).and_then(Value::as_bool).unwrap_or(false)
    }
    pub fn float(&self, key: &str) -> f64 {
        self.data.get(key).and_then(Value::as_f64).unwrap_or(0.0)
    }
    pub fn str(&self, key: &str) -> String {
        self.data
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string()
    }
    /// Есть ли ключ в data (отличить «нет данных» от нуля).
    pub fn has(&self, key: &str) -> bool {
        self.data.get(key).is_some()
    }
    /// data события light_command.
    pub fn light_command(&self) -> LightCommand {
        let d = &self.data;
        LightCommand {
            object_id: d["object_id"].as_str().unwrap_or("").to_string(),
            state: d["state"].as_bool().unwrap_or(false),
            brightness: d["brightness"].as_f64().unwrap_or(0.0),
            red: d["red"].as_f64().unwrap_or(0.0),
            green: d["green"].as_f64().unwrap_or(0.0),
            blue: d["blue"].as_f64().unwrap_or(0.0),
            effect: d["effect"].as_str().unwrap_or("").to_string(),
        }
    }
}

/// Внутреннее синтетическое событие (не от LVA): соединение потеряно.
pub const EV_DOWN: &str = "_down";

/// data события light_command.
#[derive(Clone, Debug, Default)]
pub struct LightCommand {
    pub object_id: String,
    pub state: bool,
    pub brightness: f64,
    pub red: f64,
    pub green: f64,
    pub blue: f64,
    pub effect: String,
}

/// Живое соединение: отправитель команд + identity (для «последний побеждает»).
struct ConnHandle {
    tx: mpsc::UnboundedSender<String>,
    id: Arc<()>,
}

/// Потокобезопасный отправитель команд.
#[derive(Clone)]
pub struct Client {
    slot: Arc<Mutex<Option<ConnHandle>>>,
}

impl Client {
    /// Шлёт команду; молча пропускает, если связи нет. data=None — поле data
    /// опускается, как требует дока (команды без payload шлются без data).
    pub fn send(&self, cmd: &str, data: Option<Value>) {
        let guard = self.slot.lock().unwrap();
        let Some(h) = guard.as_ref() else { return };
        let mut m = serde_json::Map::new();
        m.insert("command".into(), json!(cmd));
        if let Some(d) = data {
            m.insert("data".into(), d);
        }
        if let Err(e) = h.tx.send(Value::Object(m).to_string()) {
            logln!("lva: send {}: {}", cmd, e);
        }
    }

    /// Есть ли живой клиент LVA.
    pub fn connected(&self) -> bool {
        self.slot.lock().unwrap().is_some()
    }

    /// Объявить кольцо как light-сущность HA (идемпотентно).
    pub fn register_light(&self, name: &str, object_id: &str, effects: &[&str]) {
        self.send(
            "register_light",
            Some(json!({
                "name": name,
                "object_id": object_id,
                "effects": effects,
                "supports_rgb": true,
                "supports_brightness": true,
            })),
        );
    }
}

pub type OnConnect = Arc<dyn Fn(&Client) + Send + Sync>;

/// WS-сервер периферии: слушает addr (напр. ":6055"), LVA — клиент.
/// Пока клиентов нет, демон тихо живёт без LVA; первый коннект кладёт в канал
/// события (включая snapshot), обрыв активного клиента — EV_DOWN. Если LVA
/// подключился повторно, прежний клиент выкидывается («последний побеждает»).
pub struct Server {
    addr: String,
    slot: Arc<Mutex<Option<ConnHandle>>>,
    events: mpsc::UnboundedSender<Event>,
    on_connect: Option<OnConnect>,
}

impl Server {
    /// Конструктор: адрес прослушивания и обработчик подключения (вызывается
    /// после каждого коннекта — удобно для регистрации сущностей).
    /// Возвращает (сервер, клиент, приёмник событий).
    pub fn new(addr: impl Into<String>, on_connect: Option<OnConnect>) -> (Server, Client, mpsc::UnboundedReceiver<Event>) {
        let (tx, rx) = mpsc::unbounded_channel();
        let slot: Arc<Mutex<Option<ConnHandle>>> = Arc::new(Mutex::new(None));
        let srv = Server {
            addr: addr.into(),
            slot: Arc::clone(&slot),
            events: tx,
            on_connect,
        };
        let client = Client { slot };
        (srv, client, rx)
    }

    /// Запустить ws-сервер; блокирует, запускать задачей (забирает Server).
    pub async fn serve(self, mut ctx: watch::Receiver<bool>) {
        'listen: loop {
            if crate::util::cancelled(&ctx) {
                break;
            }
            let listener = match TcpListener::bind(&self.addr).await {
                Ok(l) => l,
                Err(e) => {
                    logln!("lva: listen {}: {} (повтор через 5с)", self.addr, e);
                    util::sleep_ctx(&mut ctx, std::time::Duration::from_secs(5)).await;
                    continue;
                }
            };
            logln!("lva: жду LVA на {}", self.addr);
            loop {
                tokio::select! {
                    _ = ctx.changed() => {
                        if crate::util::cancelled(&ctx) { break 'listen; }
                    }
                    acc = listener.accept() => {
                        let Ok((stream, peer)) = acc else { continue };
                        let slot = Arc::clone(&self.slot);
                        let events = self.events.clone();
                        let on_connect = self.on_connect.clone();
                        let ctx2 = ctx.clone();
                        tokio::spawn(async move {
                            handle_conn(slot, events, on_connect, stream, peer, ctx2).await;
                        });
                    }
                }
            }
        }
        let mut g = self.slot.lock().unwrap();
        if let Some(h) = g.take() {
            let _ = h.tx.send(String::new()); // разбудить writer, чтобы закрыл сокет
        }
    }
}

async fn handle_conn(
    slot: Arc<Mutex<Option<ConnHandle>>>,
    events: mpsc::UnboundedSender<Event>,
    on_connect: Option<OnConnect>,
    stream: TcpStream,
    peer: SocketAddr,
    ctx: watch::Receiver<bool>,
) {
    let _ = ctx; // время жизни соединения определяется самим сокетом
    let ws = match tokio_tungstenite::accept_async(stream).await {
        Ok(w) => w,
        Err(e) => {
            logln!("lva: upgrade: {}", e);
            return;
        }
    };
    let (mut sink, mut read) = ws.split();
    let (req_tx, mut req_rx) = mpsc::unbounded_channel::<String>();
    let id = Arc::new(());

    // «последний побеждает»: прежний ConnHandle выкидывается — его writer
    // завершится (rx исчерпан), sink закроется, старый read-loop отвалится
    // молча (identity не совпадёт).
    {
        let mut g = slot.lock().unwrap();
        *g = Some(ConnHandle {
            tx: req_tx.clone(),
            id: Arc::clone(&id),
        });
    }

    logln!("lva: подключено с {}", peer);

    // writer: единственный владелец sink; команды — строками JSON
    tokio::spawn(async move {
        while let Some(msg) = req_rx.recv().await {
            if sink.send(Message::text(msg)).await.is_err() {
                break;
            }
        }
        let _ = sink.close().await;
    });

    if let Some(cb) = on_connect {
        cb(&Client { slot: Arc::clone(&slot) });
    }

    let mut conn_error: Option<String> = None;
    while let Some(msg) = read.next().await {
        let raw = match msg {
            Ok(Message::Text(t)) => t.to_string(),
            Ok(Message::Binary(b)) => String::from_utf8_lossy(&b).into_owned(),
            Ok(_) => continue,
            Err(e) => {
                conn_error = Some(e.to_string());
                break;
            }
        };
        let Ok(v) = serde_json::from_str::<Value>(&raw) else {
            continue; // невалидный JSON — молча, как в Go
        };
        let event = v.get("event").and_then(Value::as_str).unwrap_or("").to_string();
        if event.is_empty() {
            continue;
        }
        let e = Event {
            event,
            data: v.get("data").cloned().unwrap_or(Value::Null),
        };
        let _ = events.send(e);
    }

    // активен ли ЭТО клиент (его могли выкинуть новым коннектом)
    let mut g = slot.lock().unwrap();
    let active = g
        .as_ref()
        .map(|h| Arc::ptr_eq(&h.id, &id))
        .unwrap_or(false);
    if active {
        *g = None;
    }
    drop(g);
    if active {
        logln!(
            "lva: клиент отвалился: {}",
            conn_error.unwrap_or_else(|| "соединение закрыто".into())
        );
        let _ = events.send(Event {
            event: EV_DOWN.to_string(),
            data: Value::Null,
        });
    }
}

#[cfg(test)]
mod tests {
    // Тесты серверной стороны: коннект клиента-«LVA», события в обе стороны,
    // EV_DOWN при обрыве, режим «последний побеждает». Порт lva_test.go.
    use std::time::Duration;

    use futures_util::{SinkExt, StreamExt};
    use serde_json::json;
    use tokio_tungstenite::tungstenite::Message;

    use super::*;

    const TEST_ADDR: &str = "127.0.0.1:60599";

    type WsStream = tokio_tungstenite::WebSocketStream<
        tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
    >;

    async fn dial() -> WsStream {
        for _ in 0..50 {
            // слушатель поднимается асинхронно
            if let Ok((ws, _)) = tokio_tungstenite::connect_async(format!("ws://{}", TEST_ADDR)).await {
                return ws;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        panic!("dial: не подключился");
    }

    async fn wait_event(ch: &mut mpsc::UnboundedReceiver<Event>, d: Duration) -> Event {
        tokio::time::timeout(d, ch.recv())
            .await
            .expect("событие не пришло")
            .expect("канал событий закрыт")
    }

    #[tokio::test]
    async fn test_serve() {
        let (ctx_tx, ctx_rx) = watch::channel(false);
        let (srv, client, mut ch) = Server::new(TEST_ADDR, None);
        tokio::spawn(async move { srv.serve(ctx_rx).await });

        let mut ws = dial().await;

        // событие от LVA доходит с разобранным data
        ws.send(Message::text(
            json!({"event": "snapshot", "data": {"muted": true}}).to_string(),
        ))
        .await
        .unwrap();
        let e = wait_event(&mut ch, Duration::from_secs(2)).await;
        assert_eq!(e.event, "snapshot");
        assert!(e.bool("muted"), "snapshot: muted должен быть true");

        // команда от демона доходит до LVA
        client.send("start_listening", None);
        let msg = tokio::time::timeout(Duration::from_secs(2), ws.next())
            .await
            .expect("команда не пришла")
            .unwrap()
            .unwrap();
        let got: Value = serde_json::from_str(msg.to_text().unwrap()).unwrap();
        assert_eq!(got["command"], "start_listening");
        assert!(got.get("data").is_none(), "data без payload опускается");

        // обрыв активного клиента → EV_DOWN
        ws.close(None).await.unwrap();
        let e = wait_event(&mut ch, Duration::from_secs(2)).await;
        assert_eq!(e.event, EV_DOWN);

        // повторный коннект: «последний побеждает», сервер жив
        let mut ws2 = dial().await;
        ws2.send(Message::text(json!({"event": "wake_word_detected"}).to_string()))
            .await
            .unwrap();
        let e = wait_event(&mut ch, Duration::from_secs(2)).await;
        assert_eq!(e.event, "wake_word_detected");

        let _ = ctx_tx.send(true);
    }
}

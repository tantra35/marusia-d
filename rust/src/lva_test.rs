// Тесты серверной стороны LVA: коннект клиента-«LVA», события в обе стороны,
// EV_DOWN при обрыве, режим «последний побеждает». Порт lva_test.go.
// Подключается из lva.rs через #[cfg(test)] #[path = "lva_test.rs"] mod tests;
// — в release-сборку отсек не попадает (аналог _test.go в Go).
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::json;
use tokio_tungstenite::tungstenite::Message;

use super::*;

const TEST_ADDR: &str = "127.0.0.1:60599";

type WsStream =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

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

// Тесты ring: лежат отдельным файлом, подключаются из ring/mod.rs через
// #[cfg(test)] #[path = "ring_test.rs"] mod tests; — в release-сборку
// отсек не попадает (аналог _test.go в Go).
use super::*;

fn st() -> RingState {
    RingState {
        scale: 1.0,
        ..RingState::default()
    }
}

#[tokio::test]
async fn frame_flash_overrides_scene() {
    let mut r = st();
    r.muted = true;
    let now = SystemTime::now();
    r.flash(Color { r: 255, g: 255, b: 255 }, Duration::from_secs(1));

    let (key, px, animated) = r.frame(now);
    assert!(key.is_empty() && animated, "want анимацию вспышки");
    assert_eq!(px[0], [255, 255, 255], "want полную яркость в начале спада");
    // к концу вспышки яркость упала
    let (_, px_end, _) = r.frame(now + Duration::from_millis(900));
    assert!(px_end[0][0] < px[0][0], "хвост вспышки ярче начала");
}

#[tokio::test]
async fn flash_actor_smoke() {
    // Сеттеры fire-and-forget: убеждаемся, что команда доходит до актора
    // (muted-запрос выстраивается в очередь за Flash — FIFO актора).
    let (ctx_tx, ctx_rx) = watch::channel(false);
    let r = Ring::new(ctx_rx);
    r.flash(WHITE, Duration::from_millis(800));
    assert!(!r.muted().await);
    let _ = ctx_tx.send(true);
}

#[tokio::test]
async fn frame_muted() {
    let mut r = st();
    r.muted = true;
    let (key, px, animated) = r.frame(SystemTime::now());
    // dim входит в ключ (иначе SetScale скипается дедупом) — проверяем префикс.
    assert!(key.starts_with("muted@") && !animated, "want muted без анимации");
    assert_eq!(px[0], MUTED_RGB);
}

/// Смена scale на статичной сцене обязана менять ключ кадра: иначе render()
/// скипает по last_drawn и новая яркость от датчика света не доедет до
/// диодов (регрессия: ключ был голым "solid"/"muted").
#[tokio::test]
async fn static_key_includes_scale() {
    let mut r = st();
    r.ha_active = true;
    r.solid = Color { r: 255, g: 255, b: 255 };
    r.scale = 1.0;
    let (key1, _, _) = r.frame(SystemTime::now());
    r.scale = 0.2;
    let (key2, _, _) = r.frame(SystemTime::now());
    assert_ne!(key1, key2, "ключ solid не изменился при смене scale, дедуп скипал бы кадр");

    r.ha_active = false;
    r.muted = true;
    r.scale = 1.0;
    let (key3, _, _) = r.frame(SystemTime::now());
    r.scale = 0.2;
    let (key4, _, _) = r.frame(SystemTime::now());
    assert_ne!(key3, key4, "ключ muted не изменился при смене scale, дедуп скипал бы кадр");
}

#[tokio::test]
async fn frame_animations_breathing() {
    let mut r = st();
    r.dialog = "listening".to_string();
    let now = SystemTime::now();
    let (key1, px1, anim1) = r.frame(now);
    let (_, px2, anim2) = r.frame(now + Duration::from_millis(400));
    assert!(key1.is_empty() && anim1 && anim2, "want анимацию дыхания");
    assert_ne!(px2[0], px1[0], "яркость дыхания не меняется между фазами");
}

#[tokio::test]
async fn frame_off() {
    let r = st();
    let (key, px, animated) = r.frame(SystemTime::now());
    assert_eq!(key, "off");
    assert!(!animated);
    assert_eq!(px[0], [0, 0, 0], "want чёрный");
}

#[tokio::test]
async fn scale_clamp() {
    let mut r = st();
    r.set_scale(5.0); // клампит в [0..1]
    assert_eq!(r.scale, 1.0, "want 1 (кламп сверху)");
    r.set_scale(-1.0);
    assert_eq!(r.scale, 0.0, "want 0 (кламп снизу)");
}

// Тесты main (шкалы громкости): подключаются из main.rs через
// #[cfg(test)] #[path = "main_test.rs"] mod tests; — в release-сборку
// отсек не попадает (аналог _test.go в Go).
use super::*;

#[test]
fn volume_color_gradient() {
    // pct=-1 — белая
    assert_eq!(volume_color(-1), ring::WHITE);
    // 0 — чисто синий (0,0,255)
    assert_eq!(volume_color(0), Color { r: 0, g: 0, b: 255 });
    // середина визуальной шкалы — зелёный (0,255,0)
    assert_eq!(volume_color(VOLUME_VISUAL_MAX / 2), Color { r: 0, g: 255, b: 0 });
    // максимум визуальной шкалы — оранжевый (255,140,0)
    assert_eq!(volume_color(VOLUME_VISUAL_MAX), Color { r: 255, g: 140, b: 0 });
}

#[test]
fn visual_scale() {
    assert_eq!(to_visual(-1), -1, "«не узнали» проходит насквозь");
    assert_eq!(to_visual(0), 0);
    assert_eq!(to_visual(75), 50);
    assert_eq!(to_visual(VOLUME_MAX_PCT), VOLUME_VISUAL_MAX);
}

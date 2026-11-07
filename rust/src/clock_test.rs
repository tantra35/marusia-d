// Тесты clock: лежат отдельным файлом, подключаются из clock.rs через
// #[cfg(test)] #[path = "clock_test.rs"] mod tests; — в release-сборку
// отсек не попадает (аналог _test.go в Go).
use super::*;

/// Глифы не задевают вывод времени: буквы 'a'..'h' не пересекаются с
/// '0'..'9' и пробелом — обычный вывод часов не затрагивается.
#[test]
fn glyphs_outside_time_charset() {
    for &(code, _) in &GLYPH_MASKS {
        assert!(!code.is_ascii_digit(), "глиф {} — цифра, заденет время", code);
        assert_ne!(code, b' ', "глиф-пробел заденет подкладку поля");
    }
}

/// face_eyes отдаёт буквы, у которых есть маска в GLYPH_MASKS.
#[test]
fn face_eyes_have_glyphs() {
    for name in [
        "idle", "neutral", "surprised", "curious", "sleepy", "shock", "wink", "angry",
    ] {
        let (l, r) = face_eyes(name).expect("лицо должно существовать");
        for &code in &[l, r] {
            assert!(
                GLYPH_MASKS.iter().any(|&(c, _)| c == code),
                "буква '{}' лица {} без маски в GLYPH_MASKS",
                code as char,
                name
            );
        }
    }
    assert!(face_eyes("nope").is_none());
}

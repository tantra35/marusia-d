// Тесты SPI-кадра: лежат отдельным файлом, подключаются из spi.rs через
// #[cfg(test)] #[path = "spi_test.rs"] mod tests; — в release-сборку
// отсек не попадает (аналог _test.go в Go).
use super::*;

/// Кадр в DMA-буфере обязан лежать с реверсом каждого 8-байтного слова:
/// DMA spicc гонит слово старшим байтом вперёд (vendor-схема ringled).
/// Без реверса каналы перемешиваются.
#[test]
fn encode_frame_word_reverse() {
    let px = [[0x10u8, 0xAA, 0x03]; LEDS]; // неравномерный цвет — ловит перестановку
    let mut frame = [0u8; FRAME_LEN];
    encode_frame_into(&mut frame, &px);
    assert_eq!(frame.len(), 400);

    // логический кадр БЕЗ реверса — сборка как на проводе (reset+GRB+reset)
    let mut logical = [0u8; FRAME_LEN];
    let mut i = RESET_LEN;
    for p in px {
        for &ch in &[p[1], p[0], p[2]] {
            let e = encode_byte(ch);
            logical[i..i + 4].copy_from_slice(&e);
            i += 4;
        }
    }

    for w in (0..FRAME_LEN).step_by(8) {
        for j in 0..8 {
            assert_eq!(
                frame[w + j],
                logical[w + 7 - j],
                "слово {}: frame[{}]={:02x}, want {:02x} (байт по реверсу слова)",
                w,
                w + j,
                frame[w + j],
                logical[w + 7 - j]
            );
        }
    }
}

#[test]
fn encode_frame_is_fixed_length() {
    let mut buf = [0u8; FRAME_LEN];
    let px = [[255u8, 0, 0]; LEDS];
    encode_frame_into(&mut buf, &px);
    assert_eq!(buf.len(), 400);
    // reset-зоны симметричны нулям — реверс их не меняет
    assert!(buf[..RESET_LEN].iter().all(|&b| b == 0));
    assert!(buf[FRAME_LEN - RESET_LEN..].iter().all(|&b| b == 0));
}

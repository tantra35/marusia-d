//! SPI-транспорт кольца: rebind контроллера + отправка кадра через spidev.
//! Порт internal/ring/spi.go (Go).
//!
//! Зависимости от железа (экспериментально проверено 2026-11-06, живая полоса):
//!   - бит → 4 SPI-бита: 0 → 1000b, 1 → 1100b («1»=1110 даёт high 0.9 мкс —
//!     вне спекции WS2812 и декодируется полосой нестабильно);
//!   - каналы внутри диода в порядке байт, как шлём (для RGB-цвета — G,R,B);
//!   - кадр = 128 reset + 144 байта (12 диодов × 12 байт) + 128 reset = 400;
//!   - speed 3 346 144 Гц, bits_per_word=8 (64-битный DMA переставляет байты
//!     внутри слов — каналы скачмаплены);
//!   - rebind контроллера (unbind/bind) перед КАЖДЫМ кадром: делитель SPI
//!     корректен только после probe, мульти-кадровые сессии не живут.
//!
//! Все вызовы — только из актора (serve). draw() блокирует поток ~30–60 мс
//! (как в Go — блокирует горутину), поэтому DMA-буферы должны жить в акторе
//! постоянно: их физические страницы не возвращаются ядру между кадрами.
use std::ffi::CString;
use std::io;
use std::process::Command;
use std::thread;
use std::time::Duration;

use crate::ring::{BYTES_PER_LED, LEDS};

pub const SPI_DEV: &str = "/dev/spidev0.0";
const FRAME_HZ: u32 = 3346144;
const PRIME_HZ: u32 = 1000000;
pub const PRIME_LEN: usize = 16;
const RESET_LEN: usize = 128;
pub const FRAME_LEN: usize = RESET_LEN * 2 + LEDS * BYTES_PER_LED; // 400
const SPI_MAJOR: u32 = 153;
const SPI_MINOR: u32 = 0;
/// пауза unbind→bind
const REBIND_SETTLE: Duration = Duration::from_millis(15);

/// Байт → 4 байта SPI: бит 0 → 1000b, бит 1 → 1100b.
pub fn encode_byte(b: u8) -> [u8; 4] {
    let mut out: u32 = 0;
    for i in (0..8).rev() {
        let n: u32 = if (b >> i) & 1 == 1 { 0xC } else { 0x8 };
        out = (out << 4) | n;
    }
    [(out >> 24) as u8, (out >> 16) as u8, (out >> 8) as u8, out as u8]
}

/// Собрать кадр в БУФЕР ДМА: reset + GRB-пиксели + reset. Буфер передаётся
/// постоянный (живает в акторе): его физические страницы не возвращаются ядру
/// между кадрами, и cache-sync при unmap всегда падает на наши же данные.
pub fn encode_frame_into(buf: &mut [u8; FRAME_LEN], px: &[[u8; 3]; LEDS]) {
    buf[..RESET_LEN].fill(0);
    let mut i = RESET_LEN;
    for p in px {
        for &ch in &[p[1], p[0], p[2]] {
            // GRB
            let e = encode_byte(ch);
            buf[i..i + 4].copy_from_slice(&e);
            i += 4;
        }
    }
    buf[i..].fill(0);
    debug_assert_eq!(i + RESET_LEN, FRAME_LEN);
}

/// spi_ioc_transfer, uapi-порядок полей (bits_per_word ПЕРЕД cs_change),
/// 32 байта — сверено с ядром 6.18.
#[repr(C)]
struct SpiXfer {
    tx_buf: u64,
    rx_buf: u64,
    len: u32,
    speed: u32,
    delay: u16,
    bits: u8,
    cs: u8,
    tx_n: u8,
    rx_n: u8,
    pad: u16,
}

const XFER_ZERO: SpiXfer = SpiXfer {
    tx_buf: 0,
    rx_buf: 0,
    len: 0,
    speed: 0,
    delay: 0,
    bits: 0,
    cs: 0,
    tx_n: 0,
    rx_n: 0,
    pad: 0,
};

fn ioc_message(n: usize) -> libc::c_ulong {
    (0x40000000u64 | ((n as u64 * 32) << 16) | (0x6Bu64 << 8)) as libc::c_ulong
}

/// rebind + открыть узел + отправить кадр. Блокирует ~30–60 мс.
pub fn draw(prime: &[u8; PRIME_LEN], frame: &[u8; FRAME_LEN]) -> io::Result<()> {
    // unbind: «уже отвязан» игнорируем; bind: свежий probe → живой делитель
    let _ = std::fs::write("/sys/bus/platform/drivers/meson-spicc/unbind", "ffd13000.spi");
    thread::sleep(REBIND_SETTLE);
    std::fs::write("/sys/bus/platform/drivers/meson-spicc/bind", "ffd13000.spi")
        .map_err(|e| io::Error::new(e.kind(), format!("bind meson-spicc: {}", e)))?;

    let fd = open_dev()?;
    let res = xfer(fd, prime, frame);
    unsafe { libc::close(fd) };
    res
}

fn xfer(fd: i32, prime: &[u8; PRIME_LEN], frame: &[u8; FRAME_LEN]) -> io::Result<()> {
    let mut tx = [
        SpiXfer {
            tx_buf: prime.as_ptr() as u64,
            len: PRIME_LEN as u32,
            speed: PRIME_HZ,
            bits: 8,
            ..XFER_ZERO
        },
        SpiXfer {
            tx_buf: frame.as_ptr() as u64,
            len: FRAME_LEN as u32,
            speed: FRAME_HZ,
            bits: 8,
            ..XFER_ZERO
        },
    ];
    let req = ioc_message(tx.len());
    let r = unsafe { libc::ioctl(fd, req.try_into().expect("ioctl request"), tx.as_mut_ptr()) };
    if r < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Открыть узел; после rebind mdev может не успеть его создать — создаём сами
/// (mknod), висячий старый узел сносим.
fn open_dev() -> io::Result<i32> {
    let path = CString::new(SPI_DEV).unwrap();
    for _ in 0..10 {
        let fd = unsafe { libc::open(path.as_ptr(), libc::O_RDWR) };
        if fd >= 0 {
            return Ok(fd);
        }
        let _ = std::fs::remove_file(SPI_DEV);
        let mode = 0o620 | libc::S_IFCHR;
        if unsafe { libc::mknod(path.as_ptr(), mode, libc::makedev(SPI_MAJOR, SPI_MINOR)) } != 0 {
            // mknod может не удаться — дать шанс mdev
            let _ = Command::new("mdev").arg("-s").status();
        }
        thread::sleep(Duration::from_millis(10));
    }
    Err(io::Error::new(
        io::ErrorKind::NotFound,
        format!("{} не открылся после rebind", SPI_DEV),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encode_frame_is_fixed_length() {
        let mut buf = [0u8; FRAME_LEN];
        let px = [[255u8, 0, 0]; LEDS];
        encode_frame_into(&mut buf, &px);
        assert_eq!(buf.len(), 400);
        // payload начинается после reset и содержит GRB-кодирование
        assert!(buf[..RESET_LEN].iter().all(|&b| b == 0));
        assert!(buf[FRAME_LEN - RESET_LEN..].iter().all(|&b| b == 0));
        // красный → G=0 → байты 1000b-повторы, R=255 → 1100b-повторы
        let first_payload = &buf[RESET_LEN..RESET_LEN + 4];
        let g_encoded = encode_byte(0);
        assert_eq!(first_payload, g_encoded);
    }
}

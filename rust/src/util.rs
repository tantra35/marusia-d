//! Мелкая общая утилита: логирование со временем (как log.Ltime в Go)
//! и sleep с учётом отмены (аналог sleepCtx).
use std::time::Duration;
use tokio::sync::watch;

/// Время HH:MM:SS — как log.Ltime в Go.
pub fn hms() -> String {
    chrono::Local::now().format("%H:%M:%S").to_string()
}

/// Спать d, но проснуться раньше, если ctx отменён (аналог sleepCtx).
pub async fn sleep_ctx(ctx: &mut watch::Receiver<bool>, d: Duration) {
    tokio::select! {
        _ = ctx.changed() => {}
        _ = tokio::time::sleep(d) => {}
    }
}

/// ctx отменён?
pub fn cancelled(ctx: &watch::Receiver<bool>) -> bool {
    *ctx.borrow()
}

/// Лог в стиле Go log.Printf с префиксом времени.
#[macro_export]
macro_rules! logln {
    ($($arg:tt)*) => {
        println!("{} {}", $crate::util::hms(), format_args!($($arg)*))
    };
}

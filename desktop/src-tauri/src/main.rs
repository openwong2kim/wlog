// wlog desktop shell.
//
// This is a thin Tauri wrapper around the existing wlog binary. wlog already
// serves its full web UI over a loopback HTTP server, so the desktop app does
// not reimplement anything: at startup it spawns wlog (bundled as a Tauri
// "sidecar"/externalBin) with --no-open --ui-port <PORT>, and the WebView loads
// the placeholder page in ../dist, which polls /api/health and redirects to the
// live server once it is up (see dist/index.html).
//
// The sidecar's CommandChild is stored in app state and killed on RunEvent::Exit
// so quitting the window also tears down the wlog server instead of leaving an
// orphaned process holding the port and the DB.

// Hide the extra console window on Windows in release builds.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::sync::Mutex;

use tauri::{Manager, RunEvent};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

/// Fixed loopback UI port the sidecar binds and the WebView loads. Kept in sync
/// with dist/index.html's redirect target.
const UI_PORT: &str = "8899";

/// Holds the running wlog sidecar so it can be killed on app exit.
struct SidecarChild(Mutex<Option<CommandChild>>);

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .setup(|app| {
            // Spawn the bundled wlog binary. `sidecar("wlog")` resolves to
            // binaries/wlog-<target-triple>(.exe) in dev and to the wlog binary
            // placed next to the app executable in a bundled install.
            let command = app
                .shell()
                .sidecar("wlog")
                .expect("wlog sidecar binary not found")
                .args(["--no-open", "--ui-port", UI_PORT]);

            let (mut rx, child) = command.spawn().expect("failed to spawn wlog sidecar");
            app.manage(SidecarChild(Mutex::new(Some(child))));

            // Drain sidecar stdout/stderr so its pipe never fills and blocks the
            // child. We don't surface logs in the UI; just keep the channel moving
            // and stop when wlog terminates.
            tauri::async_runtime::spawn(async move {
                while let Some(event) = rx.recv().await {
                    if let CommandEvent::Terminated(_) = event {
                        break;
                    }
                }
            });

            // Point the window at the server once its port accepts connections.
            // We navigate (a full page load) rather than have the placeholder
            // fetch+redirect: the WebView's origin is `tauri.localhost`, which is
            // cross-origin to the server, and wlog only emits CORS headers for
            // localhost / 127.0.0.1 origins — so a fetch from the placeholder is
            // blocked and the loading screen never advances. A navigation is not
            // subject to CORS, and once loaded the UI is same-origin with the
            // server, so all /api/* calls and the /api/now SSE work normally.
            let win = app.get_webview_window("main").expect("main window missing");
            std::thread::spawn(move || {
                use std::net::{SocketAddr, TcpStream};
                use std::time::Duration;
                let addr: SocketAddr = format!("127.0.0.1:{UI_PORT}")
                    .parse()
                    .expect("valid loopback addr");
                // Poll up to ~40s for the sidecar to bind, then load the UI.
                for _ in 0..200 {
                    if TcpStream::connect_timeout(&addr, Duration::from_millis(300)).is_ok() {
                        if let Ok(url) = format!("http://127.0.0.1:{UI_PORT}").parse::<tauri::Url>() {
                            let _ = win.navigate(url);
                        }
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(200));
                }
            });

            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application")
        .run(|app_handle, event| {
            if let RunEvent::Exit = event {
                if let Some(state) = app_handle.try_state::<SidecarChild>() {
                    if let Some(child) = state.0.lock().unwrap().take() {
                        let _ = child.kill();
                    }
                }
            }
        });
}

package tea

// Patched for kran (upstream v1.3.10 differs only in this file).
//
// Upstream's init() calls lipgloss.HasDarkBackground(), which writes an OSC 11
// query and a cursor-position request to the terminal and waits up to five
// seconds for the reply. That runs at process start, so every kran command on
// a terminal would pay it, and `kran run` would print the escape sequences
// into the output of the job it wraps. kran's TUI uses fixed colors and never
// asks for the background, so the workaround is not needed here.

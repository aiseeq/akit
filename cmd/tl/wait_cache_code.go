package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
<<<<<<< HEAD
const waitCacheCode = "43dcfbd50bc704773abf5a5fc5b878867b8157d9c789d8ba27b38811cde4f4c0"
=======
const waitCacheCode = "2ee4657e3c4744b59628131295f662af110ac795e37836a3677bf71b21ba44ac"
>>>>>>> b02d818 (fix(tl): only codex output names a running session, and a reused session id drops the abandoned wait)

package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "6c1adfb269bad53b28bcc3577f679b00fa1ae3ba6f6dea4cf7efb6a7a98d6771"

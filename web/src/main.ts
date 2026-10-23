import { createApp } from "vue"
import App from "./App.vue"
import "./assets/main.css"
import { i18n } from "./i18n"
import { useColorScheme } from "./lib/theme"

const scheme = useColorScheme()
scheme.apply(scheme.read())
document.documentElement.lang = String(i18n.global.locale.value)

createApp(App).use(i18n).mount("#app")

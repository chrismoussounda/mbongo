(() => {
  const notify = (title, body, tag, url) => {
    if (!("Notification" in window) || Notification.permission !== "granted") return;
    const n = new Notification(title, { body, tag });
    n.onclick = () => {
      window.focus();
      if (url) location.href = url;
    };
  };

  const alerts = document.getElementById("alerts");
  if (alerts && "Notification" in window && Notification.permission === "default") {
    alerts.hidden = false;
    alerts.addEventListener("click", async () => {
      await Notification.requestPermission();
      alerts.hidden = true;
    });
  }

  const list = document.querySelector("[data-feed]");
  if (list) {
    const incoming = document.getElementById("incoming");
    new EventSource(list.dataset.feed).addEventListener("prompt", (event) => {
      const p = JSON.parse(event.data);
      const title = `${p.number}: ${p.payee} asks ${p.amount}`;
      notify(title, p.operator, p.url, p.url);
      const item = document.createElement("li");
      const link = document.createElement("a");
      link.href = p.url;
      link.textContent = title;
      item.append(link);
      incoming.prepend(item);
    });
    return;
  }

  const phone = document.querySelector("[data-events]");
  if (!phone) return;

  const countdowns = () => {
    for (const el of document.querySelectorAll("[data-left]")) {
      if (!el.dataset.until) el.dataset.until = Date.now() + Number(el.dataset.left) * 1000;
      const left = Math.max(0, Math.round((Number(el.dataset.until) - Date.now()) / 1000));
      el.textContent = left > 0 ? `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")} left` : "expired";
      if (left === 0) {
        for (const button of el.closest(".ussd").querySelectorAll("button")) button.disabled = true;
      }
    }
  };

  const clocks = () => {
    const now = new Date().toLocaleTimeString("fr-FR", { hour: "2-digit", minute: "2-digit", timeZone: "Africa/Brazzaville" });
    for (const el of document.querySelectorAll("[data-clock]")) el.textContent = now;
  };

  const ring = (prompt) => {
    phone.classList.remove("ring");
    void phone.offsetWidth;
    phone.classList.add("ring");
    if (document.hidden) notify(prompt.dataset.title, prompt.dataset.body, prompt.dataset.tx);
  };

  const pageURL = () => {
    const url = new URL(location.href);
    url.searchParams.delete("notice");
    return url;
  };

  const refresh = async () => {
    const res = await fetch(pageURL(), { cache: "no-store", credentials: "same-origin" });
    if (!res.ok) {
      location.reload();
      return;
    }
    const next = new DOMParser().parseFromString(await res.text(), "text/html");
    const screen = document.getElementById("screen");
    const freshScreen = next.getElementById("screen");
    if (screen && freshScreen) {
      const top = screen.scrollTop;
      screen.replaceChildren(...freshScreen.childNodes);
      screen.scrollTop = top;
    }
    // A prompt on screen stays as it is unless another one replaced it.
    const prompt = document.getElementById("prompt");
    const fresh = next.getElementById("prompt");
    if (prompt && fresh && prompt.dataset.tx !== fresh.dataset.tx) {
      prompt.replaceWith(fresh);
      if (fresh.dataset.tx) {
        ring(fresh);
        fresh.querySelector("button")?.focus();
      }
    }
    countdowns();
    clocks();
  };

  new EventSource(phone.dataset.events).addEventListener("change", () => {
    refresh().catch(() => {});
  });
  if (document.getElementById("prompt")?.dataset.tx && !new URLSearchParams(location.search).has("notice")) {
    ring(document.getElementById("prompt"));
  }
  countdowns();
  clocks();
  setInterval(countdowns, 1000);
  setInterval(clocks, 15000);
})();

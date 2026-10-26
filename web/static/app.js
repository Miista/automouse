function App() {
  return {
    view: 'loading', // 'loading' | 'error' | 'app'
    tab: 'dashboard',

    state: {
      totals: { cumulative_upload_gb: 0, cumulative_points_spent: 0, cumulative_freeleech_wedges: 0, cumulative_vip_purchases: 0 },
      history: [],
      scheduler_enabled: false,
      paused: false,
      running: false,
      next_run_time: null,
      have_points: false,
      current_points: 0,
    },
    settings: { values: {}, env_managed: {}, mam_id_set: false },
    mamIdInput: '',
    settingsError: '',
    settingsSaved: false,
    pollTimer: null,
    runMenuOpen: false,
    schedulerError: '',
    locale: localStorage.getItem('automouse.locale') || '',

    async mounted() {
      // Closes the dry-run dropdown on any click outside the split-button.
      document.addEventListener('click', (e) => {
        if (!e.target.closest('.split-button')) this.runMenuOpen = false;
      });

      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'error';
      if (ok) this.startPolling();
    },

    // Sign-in is the auth gate's: a page load without a session is redirected
    // to the identity provider before this script ever runs, and an API call
    // without one gets 401. So a 401 here means the session ended while the
    // page was open — reloading sends the browser back through sign-in.
    async tryLoadState() {
      const res = await fetch('/api/state');
      if (res.status === 401) {
        location.reload();
        return false;
      }
      if (!res.ok) return false;
      this.state = await res.json();
      await this.loadSettings();
      return true;
    },

    async loadSettings() {
      const res = await fetch('/api/settings');
      if (!res.ok) return;
      this.settings = await res.json();
      this.mamIdInput = '';
    },

    startPolling() {
      if (this.pollTimer) return;
      this.pollTimer = setInterval(async () => {
        const res = await fetch('/api/state');
        if (res.status === 401) location.reload();
        else if (res.ok) this.state = await res.json();
      }, 5000);
    },

    async startSchedule() {
      await fetch('/api/start', { method: 'POST' });
      await this.tryLoadState();
    },

    async pauseSchedule() {
      await fetch('/api/pause', { method: 'POST' });
      await this.tryLoadState();
    },

    async runNow() {
      this.schedulerError = '';
      const res = await fetch('/api/run', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ fl_only_override: false }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.reason) this.schedulerError = data.reason || data.error || 'Failed to start the run.';
      await this.tryLoadState();
    },

    async dryRunNow() {
      this.schedulerError = '';
      const res = await fetch('/api/dry-run', { method: 'POST' });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.reason) this.schedulerError = data.reason || data.error || 'Failed to start the dry run.';
      await this.tryLoadState();
    },

    async saveSettings() {
      this.settingsError = '';
      this.settingsSaved = false;
      const payload = { ...this.settings.values };
      // alternate_next_purchase is internal scheduler rotation state, not a
      // user-editable setting — never send it back, so a save can never
      // clobber it (the API also ignores it if sent, but don't rely on that).
      delete payload.alternate_next_purchase;
      if (this.mamIdInput) {
        payload.mam_id = this.mamIdInput;
      } else {
        delete payload.mam_id;
      }
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        this.settingsError = data.error || 'Failed to save settings.';
        return;
      }
      this.settings = await res.json();
      this.mamIdInput = '';
      this.settingsSaved = true;
      setTimeout(() => { this.settingsSaved = false; }, 2500);
    },

    get statusLabel() {
      if (this.state.running) return 'Running now';
      // A rate-limit backoff outranks the scheduled/paused distinction:
      // the scheduler may well be active, but nothing will fire until the
      // window passes, and saying "Scheduled" would be misleading.
      if (this.rateLimited) return 'Rate limited';
      if (this.state.paused || !this.state.scheduler_enabled) return 'Paused';
      return 'Scheduled';
    },

    get rateLimited() {
      const until = this.state.rate_limited_until;
      return !!until && new Date(until) > new Date();
    },

    // Empty locale means "follow the browser". The locale drives clock
    // (12/24h), date order and number separators together.
    get loc() {
      return this.locale || undefined;
    },

    setLocale() {
      if (this.locale) localStorage.setItem('automouse.locale', this.locale);
      else localStorage.removeItem('automouse.locale');
    },

    formatNumber(n) {
      return (n || 0).toLocaleString(this.loc);
    },

    formatTime(value) {
      if (!value) return '—';
      const d = new Date(value);
      if (isNaN(d.getTime())) return String(value);
      return d.toLocaleString(this.loc);
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');

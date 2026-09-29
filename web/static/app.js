function App() {
  return {
    view: 'loading', // 'loading' | 'setup' | 'login' | 'app'
    tab: 'dashboard',
    loggedIn: false,

    setupForm: { username: '', password: '' },
    setupError: '',
    loginForm: { username: '', password: '' },
    loginError: '',

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

    async mounted() {
      const setupRes = await fetch('/api/setup');
      const setupData = await setupRes.json();
      if (!setupData.admin_exists) {
        this.view = 'setup';
        return;
      }
      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'login';
      if (ok) this.startPolling();
    },

    async tryLoadState() {
      const res = await fetch('/api/state');
      if (res.status === 401 || res.status === 412) return false;
      if (!res.ok) return false;
      this.state = await res.json();
      this.loggedIn = true;
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
        if (res.ok) this.state = await res.json();
      }, 5000);
    },

    async submitSetup() {
      this.setupError = '';
      const res = await fetch('/api/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(this.setupForm),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        this.setupError = data.error || 'Setup failed.';
        return;
      }
      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'login';
      if (ok) this.startPolling();
    },

    async submitLogin() {
      this.loginError = '';
      const res = await fetch('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(this.loginForm),
      });
      if (!res.ok) {
        this.loginError = 'Invalid username or password.';
        return;
      }
      const ok = await this.tryLoadState();
      this.view = ok ? 'app' : 'login';
      if (ok) this.startPolling();
    },

    async logout() {
      await fetch('/api/logout', { method: 'POST' });
      if (this.pollTimer) {
        clearInterval(this.pollTimer);
        this.pollTimer = null;
      }
      this.loggedIn = false;
      this.view = 'login';
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
      await fetch('/api/run', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ fl_only_override: false }),
      });
      await this.tryLoadState();
    },

    async saveSettings() {
      this.settingsError = '';
      this.settingsSaved = false;
      const payload = { ...this.settings.values };
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
      if (this.state.paused || !this.state.scheduler_enabled) return 'Paused';
      return 'Scheduled';
    },

    formatTime(value) {
      if (!value) return '—';
      const d = new Date(value);
      if (isNaN(d.getTime())) return String(value);
      return d.toLocaleString();
    },
  };
}

PetiteVue.createApp({ App }).mount('#app');

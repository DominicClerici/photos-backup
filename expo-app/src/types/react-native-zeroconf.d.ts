declare module 'react-native-zeroconf' {
  export type ZeroconfService = {
    name: string;
    fullName?: string;
    host?: string;
    port?: number;
    addresses?: string[];
    txt?: Record<string, string>;
  };

  export type ZeroconfEvents = {
    start: () => void;
    stop: () => void;
    found: (name: string) => void;
    remove: (name: string) => void;
    resolved: (service: ZeroconfService) => void;
    update: () => void;
    error: (err: Error) => void;
  };

  export default class Zeroconf {
    on<E extends keyof ZeroconfEvents>(event: E, listener: ZeroconfEvents[E]): this;
    off<E extends keyof ZeroconfEvents>(event: E, listener: ZeroconfEvents[E]): this;
    removeAllListeners(event?: keyof ZeroconfEvents): this;
    scan(type?: string, protocol?: string, domain?: string): void;
    stop(): void;
    getServices(): Record<string, ZeroconfService>;
    publishService(
      type: string,
      protocol: string,
      domain: string,
      name: string,
      port: number,
      txt?: Record<string, string>
    ): void;
    unpublishService(name: string): void;
    addDeviceListeners(): void;
    removeDeviceListeners(): void;
  }
}

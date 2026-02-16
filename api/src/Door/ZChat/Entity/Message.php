<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Entity\Enum\MessageType;
use App\Door\ZChat\Repository\MessageRepository;
use App\Entity\User;
use Doctrine\DBAL\Types\Types;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: MessageRepository::class)]
#[ORM\Table(name: 'zchat_message')]
#[ORM\Index(columns: ['room_id', 'created_at'], name: 'idx_zchat_message_room_order')]
#[ORM\Index(columns: ['sender_id', 'created_at'], name: 'idx_zchat_message_sender_order')]
class Message
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne(inversedBy: 'messages')]
    #[ORM\JoinColumn(nullable: false)]
    private Room $room;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $sender;

    #[ORM\Column(type: Types::STRING, length: 20, enumType: MessageType::class)]
    private MessageType $messageType = MessageType::NORMAL;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: true)]
    private ?User $targetUser = null;

    #[ORM\Column(type: Types::TEXT)]
    private string $content;

    #[ORM\Column(type: Types::JSON, nullable: true)]
    private ?array $metadata = null;

    #[ORM\Column]
    private bool $isDeleted = false;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getRoom(): Room
    {
        return $this->room;
    }

    public function setRoom(Room $room): static
    {
        $this->room = $room;
        return $this;
    }

    public function getSender(): User
    {
        return $this->sender;
    }

    public function setSender(User $sender): static
    {
        $this->sender = $sender;
        return $this;
    }

    public function getMessageType(): MessageType
    {
        return $this->messageType;
    }

    public function setMessageType(MessageType $messageType): static
    {
        $this->messageType = $messageType;
        return $this;
    }

    public function getTargetUser(): ?User
    {
        return $this->targetUser;
    }

    public function setTargetUser(?User $targetUser): static
    {
        $this->targetUser = $targetUser;
        return $this;
    }

    public function getContent(): string
    {
        return $this->content;
    }

    public function setContent(string $content): static
    {
        $this->content = $content;
        return $this;
    }

    public function getMetadata(): ?array
    {
        return $this->metadata;
    }

    public function setMetadata(?array $metadata): static
    {
        $this->metadata = $metadata;
        return $this;
    }

    public function isDeleted(): bool
    {
        return $this->isDeleted;
    }

    public function setIsDeleted(bool $isDeleted): static
    {
        $this->isDeleted = $isDeleted;
        return $this;
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }

    public function isWhisper(): bool
    {
        return $this->messageType === MessageType::WHISPER;
    }

    public function isAction(): bool
    {
        return $this->messageType === MessageType::ACTION;
    }

    public function isSystem(): bool
    {
        return $this->messageType === MessageType::SYSTEM;
    }
}
